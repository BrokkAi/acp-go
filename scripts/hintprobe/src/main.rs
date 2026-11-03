//! Decodes JSON documents with the Rust schema crate's types and reports the
//! outcome, so the Go decoders can be compared with it.
//!
//! Reads one request per line, `{"id", "pkg", "type", "input"}`, and writes
//! one response per line: `{"id", "ok": <re-encoded value>}`,
//! `{"id", "error": <message>}`, or `{"id", "missing": true}` for a type the
//! dispatch table does not know.

use std::io::{BufRead, Write};

mod dispatch;

/// Decodes `input` as `T` the way the Rust SDK reads a message (the line is
/// parsed into a `serde_json::Value` first, so a duplicate key keeps its last
/// value, and the payload is decoded from that value), and re-encodes the
/// value.
pub fn run<T: serde::de::DeserializeOwned + serde::Serialize>(input: &str) -> Result<String, String> {
    let value = serde_json::from_str::<serde_json::Value>(input).map_err(|e| e.to_string())?;
    match serde_json::from_value::<T>(value) {
        Ok(value) => serde_json::to_string(&value).map_err(|e| format!("encode: {e}")),
        Err(e) => Err(e.to_string()),
    }
}

fn main() {
    let stdin = std::io::stdin();
    let mut out = std::io::stdout().lock();
    for line in stdin.lock().lines() {
        let line = line.expect("read request");
        if line.trim().is_empty() {
            continue;
        }
        let req: serde_json::Value = serde_json::from_str(&line).expect("request is JSON");
        let id = &req["id"];
        let name = req["type"].as_str().expect("request type");
        let input = req["input"].as_str().expect("request input");
        let result = match req["pkg"].as_str() {
            Some("v1") => dispatch::v1(name, input),
            #[cfg(feature = "unstable_protocol_v2")]
            Some("v2") => dispatch::v2(name, input),
            _ => None,
        };
        let resp = match result {
            None => serde_json::json!({"id": id, "missing": true}),
            Some(Ok(value)) => {
                let value: serde_json::Value = serde_json::from_str(&value).expect("re-encoded JSON");
                serde_json::json!({"id": id, "ok": value})
            }
            Some(Err(error)) => serde_json::json!({"id": id, "error": error}),
        };
        writeln!(out, "{resp}").expect("write response");
    }
}
