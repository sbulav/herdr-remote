import json
import datetime
import pathlib
import re
import unittest
import urllib.parse
import uuid


ROOT = pathlib.Path(__file__).parents[1]
FIXTURE = ROOT / "tests" / "fixtures" / "http_api_v1.json"
SCHEMA = ROOT / "protocol" / "http-v1.schema.json"


class HTTPContractTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.fixture = json.loads(FIXTURE.read_text())
        cls.schema = json.loads(SCHEMA.read_text())

    def test_every_endpoint_example_validates_against_real_schemas(self):
        endpoints = self.schema["x-endpoints"]
        self.assertEqual(set(self.fixture["cases"]), set(endpoints))
        for name, case in self.fixture["cases"].items():
            endpoint = endpoints[name]
            request_schema = endpoint["request"]
            if request_schema is None:
                self.assertIsNone(case["request"], name)
            else:
                self.assertTrue(validate(case["request"], resolve(self.schema, request_schema), self.schema), name)
            response_schema = resolve(self.schema, endpoint["responses"][str(case["status"])])
            self.assertTrue(validate(case["response"], response_schema, self.schema), name)

    def test_schemas_reject_missing_extra_wrong_and_oversized_fields(self):
        defs = self.schema["$defs"]
        invalid = [
            (defs["sessionResponse"], {"authenticated": True, "operator": {"display_name": "operator"}, "push_public_key": None, "logout_url": "https://id.example/logout", "issuer": "secret"}),
            (defs["csrfResponse"], {"token": "short"}),
            (defs["pushSubscription"], {"endpoint": "http://push.example", "keys": {"p256dh": "key", "auth": "auth"}}),
            (defs["pushSubscription"], {"endpoint": "https://127.0.0.1/push", "keys": {"p256dh": "key", "auth": "auth"}}),
            (defs["pushSubscription"], {"endpoint": "https://user@push.example/push", "keys": {"p256dh": "key", "auth": "auth"}}),
            (defs["pushSubscription"], {"endpoint": "https://push.example:8443/push", "keys": {"p256dh": "key", "auth": "auth"}}),
            (defs["pushSubscription"], {"endpoint": "https://push.example/push#fragment", "keys": {"p256dh": "key", "auth": "auth"}}),
            (defs["pushSubscription"], {"endpoint": "https://push.example", "keys": {"p256dh": "key"}}),
            (defs["endpointRequest"], {"endpoint": "https://push.example", "other": True}),
            (defs["endpointsRequest"], {"endpoints": []}),
            (defs["endpointsRequest"], {"endpoints": ["https://push.example/old", "https://push.example/old"]}),
            (defs["replaceRequest"], {"source_endpoints": [], "subscription": {"endpoint": "https://push.example/new", "keys": {"p256dh": "key", "auth": "auth"}}}),
            (defs["replaceRequest"], {"source_endpoints": ["https://push.example/old", "https://push.example/old"], "subscription": {"endpoint": "https://push.example/new", "keys": {"p256dh": "key", "auth": "auth"}}}),
            (defs["reconcileResponse"], {"subscribed": "yes"}),
            (defs["emptyRequest"], {"unexpected": True}),
            (defs["logoutResponse"], {"logout_url": "http://id.example/logout"}),
            (defs["logoutResponse"], {"logout_url": "https://user@id.example/logout"}),
        ]
        for schema, value in invalid:
            self.assertFalse(validate(value, schema, self.schema), value)

    def test_security_and_privacy_metadata_is_closed(self):
        endpoints = self.schema["x-endpoints"]
        for name, endpoint in endpoints.items():
            if endpoint["method"] == "GET":
                self.assertFalse(endpoint["origin_required"], name)
                continue
            self.assertTrue(endpoint["origin_required"], name)
            self.assertTrue(endpoint["csrf_required"], name)
        encoded = json.dumps(self.fixture).lower()
        for forbidden in ("oidc_token", "access_token", "issuer", "audience", "assurance"):
            self.assertNotIn(forbidden, encoded)


def resolve(root, reference):
    value = root
    for part in reference.removeprefix("#/").split("/"):
        value = value[part]
    return value


def validate(value, schema, root):
    if "$ref" in schema:
        return validate(value, resolve(root, schema["$ref"]), root)
    if "const" in schema and value != schema["const"]:
        return False
    if "enum" in schema and value not in schema["enum"]:
        return False
    types = schema.get("type")
    if types is not None:
        if not isinstance(types, list):
            types = [types]
        matches = {
            "object": isinstance(value, dict),
            "array": isinstance(value, list),
            "string": isinstance(value, str),
            "boolean": isinstance(value, bool),
            "null": value is None,
            "integer": isinstance(value, int) and not isinstance(value, bool),
        }
        if not any(matches.get(kind, False) for kind in types):
            return False
    if isinstance(value, dict):
        properties = schema.get("properties", {})
        if any(field not in value for field in schema.get("required", [])):
            return False
        if schema.get("additionalProperties") is False and any(field not in properties for field in value):
            return False
        return all(field not in properties or validate(item, properties[field], root) for field, item in value.items())
    if isinstance(value, list):
        if len(value) < schema.get("minItems", 0) or len(value) > schema.get("maxItems", len(value)):
            return False
        if schema.get("uniqueItems") and len({json.dumps(item, sort_keys=True) for item in value}) != len(value):
            return False
        return "items" not in schema or all(validate(item, schema["items"], root) for item in value)
    if isinstance(value, str):
        if len(value) < schema.get("minLength", 0) or len(value) > schema.get("maxLength", len(value)):
            return False
        if "pattern" in schema and re.search(schema["pattern"], value) is None:
            return False
        if schema.get("format") == "uri":
            parsed = urllib.parse.urlparse(value)
            if not parsed.scheme or not parsed.netloc:
                return False
        if schema.get("format") == "uuid":
            try:
                uuid.UUID(value)
            except ValueError:
                return False
        if schema.get("format") == "date-time":
            try:
                datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
            except ValueError:
                return False
    return True


if __name__ == "__main__":
    unittest.main()
