import datetime
import hashlib
import json
import pathlib
import re
import unittest
import uuid


FIXTURE = pathlib.Path(__file__).parent / "fixtures" / "connector_protocol_v1.ndjson"
OPERATIONS_FIXTURE = FIXTURE.with_name("connector_protocol_v1_operations.json")
HERDR_FIXTURE = FIXTURE.with_name("herdr_protocol_16.ndjson")
ACTION_SCHEMA = FIXTURE.parents[2] / "protocol" / "connector-v1-action.schema.json"


class ConnectorProtocolFixtureTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.messages = [json.loads(line) for line in FIXTURE.read_text().splitlines() if line]
        cls.operations = json.loads(OPERATIONS_FIXTURE.read_text())
        cls.herdr_messages = [
            json.loads(line) for line in HERDR_FIXTURE.read_text().splitlines() if line
        ]
        cls.action_schema = json.loads(ACTION_SCHEMA.read_text())

    def test_envelopes_are_well_formed_and_unique(self):
        message_ids = set()
        for message in self.messages:
            self.assertIsInstance(message["type"], str)
            self.assertIsInstance(message["body"], dict)
            message_uuid = uuid.UUID(message["message_id"])
            self.assertEqual(message_uuid.version, 7)
            self.assertEqual(message_uuid.variant, uuid.RFC_4122)
            sent_at = datetime.datetime.fromisoformat(
                message["sent_at"].replace("Z", "+00:00")
            )
            self.assertEqual(sent_at.utcoffset(), datetime.timedelta(0))
            uuid_timestamp_ms = int(message["message_id"].replace("-", "")[:12], 16)
            sent_timestamp_ms = int(sent_at.timestamp() * 1000)
            self.assertLess(abs(uuid_timestamp_ms - sent_timestamp_ms), 60_000)
            self.assertNotIn(message["message_id"], message_ids)
            message_ids.add(message["message_id"])

    def test_handshake_precedes_state(self):
        self.assertEqual(
            [message["type"] for message in self.messages[:3]],
            ["connector.hello", "server.welcome", "state.snapshot"],
        )
        hello = self.messages[0]["body"]
        welcome = self.messages[1]["body"]
        self.assertEqual(self.messages[0]["protocol"], 0)
        self.assertEqual(self.messages[1]["protocol"], 0)
        self.assertTrue(all(message["protocol"] == 1 for message in self.messages[2:]))
        self.assertLessEqual(hello["min_protocol"], welcome["selected_protocol"])
        self.assertGreaterEqual(hello["max_protocol"], welcome["selected_protocol"])
        self.assertTrue(
            set(welcome["accepted_capabilities"]).issubset(hello["capabilities"])
        )

    def test_state_delta_continues_snapshot_epoch(self):
        snapshot = next(message["body"] for message in self.messages if message["type"] == "state.snapshot")
        delta = next(message["body"] for message in self.messages if message["type"] == "state.delta")
        snapshot_epoch = uuid.UUID(snapshot["epoch"])
        self.assertEqual(snapshot_epoch.version, 7)
        self.assertEqual(snapshot_epoch.variant, uuid.RFC_4122)
        self.assertEqual(snapshot["sequence"], 0)
        self.assertEqual(delta["epoch"], snapshot["epoch"])
        self.assertEqual(delta["sequence"], snapshot["sequence"] + 1)
        self.assertGreater(
            delta["changes"][0]["agent"]["generation"],
            snapshot["agents"][0]["generation"],
        )

    def test_action_lifecycle_uses_one_id(self):
        action_messages = [
            message for message in self.messages if message["type"].startswith("action.")
        ]
        self.assertEqual(
            [message["type"] for message in action_messages],
            ["action.request", "action.result"],
        )
        action_ids = {message["body"]["action_id"] for message in action_messages}
        self.assertEqual(len(action_ids), 1)
        uuid.UUID(action_ids.pop())
        self.assertEqual(action_messages[-1]["body"]["status"], "rejected")
        self.assertEqual(action_messages[-1]["body"]["code"], "HERDR_INCOMPATIBLE")

        snapshot = next(
            message["body"] for message in self.messages if message["type"] == "state.snapshot"
        )
        self.assertNotIn("checked_input.v1", snapshot["capabilities"])

    def test_prompt_response_is_bound_to_snapshot(self):
        prompt = next(message["body"] for message in self.messages if message["type"] == "prompt.snapshot")
        action = next(message["body"] for message in self.messages if message["type"] == "action.request")
        self.assertEqual(action["expected"]["state_epoch"], prompt["state_epoch"])
        self.assertEqual(action["expected"]["agent_generation"], prompt["agent_generation"])
        self.assertEqual(action["expected"]["herdr_input_revision"], 0)
        self.assertEqual(action["expected"]["prompt_fingerprint"], prompt["fingerprint"])
        self.assertEqual(action["expected"]["herdr_content_hash"], prompt["herdr_content_hash"])
        option_ids = {option["id"] for option in prompt["options"]}
        self.assertIn(action["operation"]["option_id"], option_ids)

        self.assertFalse(prompt["excerpt_truncated"])
        canonical_prompt = {
            "v": 1,
            "host_id": prompt["target"]["host_id"],
            "instance_id": prompt["target"]["instance_id"],
            "terminal_id": prompt["target"]["terminal_id"],
            "adapter_version": prompt["adapter_version"],
            "prompt": prompt["excerpt"],
            "options": prompt["options"],
        }
        canonical_json = json.dumps(
            canonical_prompt,
            ensure_ascii=False,
            separators=(",", ":"),
            sort_keys=True,
        )
        fingerprint = "sha256:" + hashlib.sha256(canonical_json.encode()).hexdigest()
        self.assertEqual(prompt["fingerprint"], fingerprint)
        source_hash = "sha256:" + hashlib.sha256(prompt["excerpt"].encode()).hexdigest()
        self.assertEqual(prompt["herdr_content_hash"], source_hash)

    def test_every_operation_defines_success_and_failure_outcomes(self):
        expected_operations = {
            "agent.read",
            "agent.send_text",
            "agent.send_keys",
            "agent.send_input",
            "agent.interrupt",
            "prompt.respond",
        }
        self.assertEqual({case["type"] for case in self.operations}, expected_operations)
        for case in self.operations:
            self.assertEqual(case["success"]["status"], "succeeded")
            self.assertIn(case["pre_execution_failure"]["status"], {"rejected", "failed"})
            self.assertEqual(
                case["timeout_before_execution"],
                {"status": "rejected", "code": "DEADLINE_EXCEEDED"},
            )
            if case["type"] == "agent.read":
                self.assertEqual(
                    case["timeout_after_execution"],
                    {"status": "failed", "code": "DEADLINE_EXCEEDED"},
                )
                self.assertEqual(
                    case["disconnect"],
                    {"status": "failed", "code": "CONNECTION_LOST"},
                )
            else:
                self.assertEqual(
                    case["timeout_after_execution"],
                    {"status": "unknown", "code": "OUTCOME_UNKNOWN"},
                )
                self.assertEqual(
                    case["disconnect"],
                    {"status": "unknown", "code": "OUTCOME_UNKNOWN"},
                )

    def test_action_schema_covers_fixtures_and_has_resolvable_refs(self):
        operation_schemas = self.action_schema["$defs"]["operation"]["oneOf"]
        schema_operations = {
            operation["properties"]["type"]["const"] for operation in operation_schemas
        }
        fixture_operations = {case["type"] for case in self.operations}
        self.assertEqual(schema_operations, fixture_operations)
        self.assertEqual(
            set(self.action_schema["$defs"]["action_result_body"]["properties"]["operation_type"]["enum"]),
            fixture_operations,
        )

        definitions = self.action_schema["$defs"]

        def check_refs(node):
            if isinstance(node, dict):
                if "$ref" in node:
                    self.assertTrue(node["$ref"].startswith("#/$defs/"))
                    self.assertIn(node["$ref"].removeprefix("#/$defs/"), definitions)
                for value in node.values():
                    check_refs(value)
            elif isinstance(node, list):
                for value in node:
                    check_refs(value)

        check_refs(self.action_schema)

        action_request = next(
            message["body"] for message in self.messages if message["type"] == "action.request"
        )
        self.assertEqual(
            set(action_request),
            {"action_id", "target", "timeout_ms", "expected", "operation"},
        )
        self.assertEqual(
            set(action_request["expected"]),
            {
                "state_epoch",
                "agent_generation",
                "herdr_input_revision",
                "agent",
                "statuses",
                "prompt_fingerprint",
                "herdr_content_hash",
            },
        )

        state_epoch_schema = self.action_schema["$defs"]["state_epoch"]
        self.assertEqual(state_epoch_schema["$ref"], "#/$defs/uuid_v7")

    def test_all_connector_epoch_values_are_uuidv7(self):
        epoch_values = []

        def collect(node):
            if isinstance(node, dict):
                for key, value in node.items():
                    if key in {"epoch", "state_epoch", "expected_epoch"} and value is not None:
                        epoch_values.append(value)
                    collect(value)
            elif isinstance(node, list):
                for value in node:
                    collect(value)

        collect(self.messages)
        collect(self.operations)
        self.assertGreater(len(epoch_values), 0)
        epoch_pattern = re.compile(self.action_schema["$defs"]["uuid_v7"]["pattern"])
        for value in epoch_values:
            parsed = uuid.UUID(value)
            self.assertEqual(parsed.version, 7, value)
            self.assertEqual(parsed.variant, uuid.RFC_4122, value)
            self.assertIsNotNone(epoch_pattern.fullmatch(value))
        self.assertIsNone(
            epoch_pattern.fullmatch("123e4567-e89b-42d3-a456-426614174000")
        )

    def test_action_schema_rejects_controls_and_bounds_utf8(self):
        text_schema = self.action_schema["$defs"]["text"]
        self.assertEqual(text_schema["maxLength"], 4096)
        control_pattern = re.compile(text_schema["not"]["pattern"])
        for invalid in ("safe\n", "safe\t", "safe\x1b"):
            self.assertIsNotNone(control_pattern.search(invalid))
        self.assertIsNone(control_pattern.search("safe input"))

        largest_text = "\U0001f642" * text_schema["maxLength"]
        self.assertEqual(len(largest_text), text_schema["maxLength"])
        self.assertLessEqual(len(largest_text.encode()), 16_384)

        read_text_schema = self.action_schema["$defs"]["read_result"]["properties"]["text"]
        largest_output = "\U0001f642" * read_text_schema["maxLength"]
        self.assertLessEqual(len(largest_output.encode()), 131_072)

    def test_text_operations_have_no_control_characters(self):
        for case in self.operations:
            text = case["request"].get("text", "")
            self.assertTrue(
                all(ord(char) > 0x1F and not 0x7F <= ord(char) <= 0x9F for char in text)
            )

    def test_output_revisions_match_exact_text(self):
        output = next(
            message["body"] for message in self.messages if message["type"] == "output.snapshot"
        )
        revision = "sha256:" + hashlib.sha256(output["text"].encode()).hexdigest()
        self.assertEqual(output["content_revision"], revision)

        read_case = next(case for case in self.operations if case["type"] == "agent.read")
        read_result = read_case["success"]["result"]
        revision = "sha256:" + hashlib.sha256(read_result["text"].encode()).hexdigest()
        self.assertEqual(read_result["content_revision"], revision)

    def test_herdr_spike_pairs_requests_and_responses(self):
        requests = {
            message["id"]: message
            for message in self.herdr_messages
            if "method" in message
        }
        responses = {
            message["id"]: message
            for message in self.herdr_messages
            if "result" in message
        }
        self.assertEqual(set(requests), set(responses))
        self.assertEqual(
            {request["method"] for request in requests.values()},
            {"ping", "session.snapshot", "events.subscribe", "agent.get", "pane.read"},
        )
        self.assertEqual(responses["probe:ping"]["result"]["protocol"], 16)
        self.assertEqual(
            responses["probe:subscribe"]["result"]["type"], "subscription_started"
        )
        self.assertEqual(responses["probe:read"]["result"]["read"]["revision"], 0)


if __name__ == "__main__":
    unittest.main()
