import unittest

import capture_hint_fixtures as capture


class CaptureHintFixturesTests(unittest.TestCase):
    def test_dispatch_lists_each_type_once_per_protocol(self):
        source = capture.dispatch_source({"v1": {"B", "A"}, "v2": {"A"}})
        self.assertIn('"A" => Some(run::<acp::v1::A>(input)),', source)
        self.assertIn('"B" => Some(run::<acp::v1::B>(input)),', source)
        self.assertIn('"A" => Some(run::<acp::v2::A>(input)),', source)
        self.assertLess(source.index("acp::v1::A"), source.index("acp::v1::B"))
        self.assertIn('#[cfg(feature = "unstable_protocol_v2")]\npub fn v2(', source)

    def test_dispatch_uses_the_rust_path_of_types_outside_the_module(self):
        source = capture.dispatch_source({"v1": {"ProtocolVersion"}, "v2": {"ErrorData"}})
        self.assertIn('"ProtocolVersion" => Some(run::<acp::ProtocolVersion>(input)),', source)
        self.assertIn('"ErrorData" => Some(run::<serde_json::Value>(input)),', source)
        self.assertNotIn("acp::v1::ProtocolVersion", source)

    def test_dispatch_without_types_still_compiles_both_tables(self):
        source = capture.dispatch_source({})
        self.assertIn("pub fn v1(", source)
        self.assertIn("pub fn v2(", source)

    def test_fixtures_record_value_encoding_or_error(self):
        header = {"pin": "schema-v1.0.0", "pkg": "v1"}
        requests = [
            {"id": "A.x/valid", "pkg": "v1", "type": "A", "input": '{"x": 1}'},
            {"id": "A.x/null", "pkg": "v1", "type": "A", "input": '{"x": null}'},
        ]
        responses = {
            "A.x/valid": {"id": "A.x/valid", "encoded": '{"z":1,"x":18446744073709551615}'},
            "A.x/null": {"id": "A.x/null", "error": "invalid type: null"},
        }
        doc = capture.fixtures(header, requests, responses)
        self.assertEqual(doc["pin"], "schema-v1.0.0")
        self.assertEqual(doc["protocol"], "v1")
        # The encoding stays the string the probe wrote: never re-parsed,
        # so its member order and numbers are Rust's.
        self.assertEqual(doc["cases"]["A.x/valid"], {"input": '{"x": 1}', "encoded": '{"z":1,"x":18446744073709551615}'})
        self.assertEqual(doc["cases"]["A.x/null"], {"input": '{"x": null}', "error": "invalid type: null"})

    def test_fixtures_reject_types_the_probe_lacks(self):
        requests = [{"id": "Z/valid", "pkg": "v1", "type": "Z", "input": "{}"}]
        with self.assertRaises(ValueError):
            capture.fixtures({"pin": "p", "pkg": "v1"}, requests, {"Z/valid": {"id": "Z/valid", "missing": True}})

    def test_every_package_has_a_probe_build(self):
        for package, build in capture.PACKAGES.items():
            self.assertIn(build, capture.FEATURES, package)


if __name__ == "__main__":
    unittest.main()
