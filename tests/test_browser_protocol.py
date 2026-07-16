import copy
import datetime
import hashlib
import json
import pathlib
import re
import unittest
import uuid


FIXTURES = pathlib.Path(__file__).parent / "fixtures"
LIFECYCLE_FIXTURE = FIXTURES / "browser_protocol_v1.ndjson"
FAILURES_FIXTURE = FIXTURES / "browser_protocol_v1_failures.json"
SCHEMA_PATH = FIXTURES.parents[1] / "protocol" / "browser-v1.schema.json"


class Draft202012SubsetValidator:
    """Validate the JSON Schema features used by the browser v1 contract."""

    def __init__(self, schema):
        self.root = schema

    def errors(self, instance, schema=None, path="$"):
        schema = self.root if schema is None else schema
        errors = []

        if "$ref" in schema:
            reference = schema["$ref"]
            if not reference.startswith("#/$defs/"):
                return [f"{path}: non-local reference {reference}"]
            definition = reference.removeprefix("#/$defs/")
            return self.errors(instance, self.root["$defs"][definition], path)

        if "allOf" in schema:
            for child in schema["allOf"]:
                errors.extend(self.errors(instance, child, path))

        if "oneOf" in schema:
            matches = sum(not self.errors(instance, child, path) for child in schema["oneOf"])
            if matches != 1:
                errors.append(f"{path}: expected one oneOf match, got {matches}")

        if "anyOf" in schema and not any(
            not self.errors(instance, child, path) for child in schema["anyOf"]
        ):
            errors.append(f"{path}: did not match anyOf")

        if "not" in schema and not self.errors(instance, schema["not"], path):
            errors.append(f"{path}: matched forbidden schema")

        if "if" in schema:
            branch = "then" if not self.errors(instance, schema["if"], path) else "else"
            if branch in schema:
                errors.extend(self.errors(instance, schema[branch], path))

        if "const" in schema and instance != schema["const"]:
            errors.append(f"{path}: expected constant {schema['const']!r}")
        if "enum" in schema and instance not in schema["enum"]:
            errors.append(f"{path}: value is not in enum")

        expected_type = schema.get("type")
        if expected_type is not None:
            allowed_types = [expected_type] if isinstance(expected_type, str) else expected_type
            if not any(self._has_type(instance, item) for item in allowed_types):
                errors.append(f"{path}: expected type {expected_type!r}")
                return errors

        if isinstance(instance, dict):
            required = schema.get("required", [])
            for name in required:
                if name not in instance:
                    errors.append(f"{path}: missing required property {name}")

            properties = schema.get("properties", {})
            for name, value in instance.items():
                if name in properties:
                    errors.extend(self.errors(value, properties[name], f"{path}.{name}"))
                elif schema.get("additionalProperties") is False:
                    errors.append(f"{path}: unknown property {name}")

        if isinstance(instance, list):
            if len(instance) < schema.get("minItems", 0):
                errors.append(f"{path}: too few items")
            if "maxItems" in schema and len(instance) > schema["maxItems"]:
                errors.append(f"{path}: too many items")
            if schema.get("uniqueItems"):
                encoded = [json.dumps(item, sort_keys=True) for item in instance]
                if len(encoded) != len(set(encoded)):
                    errors.append(f"{path}: duplicate items")
            if "contains" in schema and not any(
                not self.errors(value, schema["contains"], path)
                for value in instance
            ):
                errors.append(f"{path}: no item matches contains")
            if "items" in schema:
                for index, value in enumerate(instance):
                    errors.extend(self.errors(value, schema["items"], f"{path}[{index}]"))

        if isinstance(instance, str):
            if len(instance) < schema.get("minLength", 0):
                errors.append(f"{path}: string is too short")
            if "maxLength" in schema and len(instance) > schema["maxLength"]:
                errors.append(f"{path}: string is too long")
            if "pattern" in schema and re.search(schema["pattern"], instance) is None:
                errors.append(f"{path}: string does not match pattern")
            if schema.get("format") == "date-time":
                try:
                    parsed = datetime.datetime.fromisoformat(instance.replace("Z", "+00:00"))
                except ValueError:
                    errors.append(f"{path}: invalid date-time")
                else:
                    if parsed.utcoffset() != datetime.timedelta(0):
                        errors.append(f"{path}: date-time is not UTC")

        if isinstance(instance, (int, float)) and not isinstance(instance, bool):
            if instance < schema.get("minimum", instance):
                errors.append(f"{path}: number is below minimum")
            if instance > schema.get("maximum", instance):
                errors.append(f"{path}: number is above maximum")

        return errors

    @staticmethod
    def _has_type(instance, expected):
        checks = {
            "array": lambda value: isinstance(value, list),
            "boolean": lambda value: isinstance(value, bool),
            "integer": lambda value: isinstance(value, int) and not isinstance(value, bool),
            "null": lambda value: value is None,
            "number": lambda value: isinstance(value, (int, float)) and not isinstance(value, bool),
            "object": lambda value: isinstance(value, dict),
            "string": lambda value: isinstance(value, str),
        }
        return checks[expected](instance)


class BrowserProtocolFixtureTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.schema = json.loads(SCHEMA_PATH.read_text())
        cls.validator = Draft202012SubsetValidator(cls.schema)
        cls.messages = [
            json.loads(line) for line in LIFECYCLE_FIXTURE.read_text().splitlines() if line
        ]
        cls.failures = json.loads(FAILURES_FIXTURE.read_text())
        cls.scenario_messages = [
            message
            for scenario in cls.failures["scenarios"]
            for message in scenario["messages"]
        ]
        cls.error_responses = [
            case["response"] for case in cls.failures["invalid_frames"]
        ]
        cls.action_recovery = [
            case["response"] for case in cls.failures["action_recovery"]
        ]

    def test_protocol_files_are_ascii(self):
        for path in (
            SCHEMA_PATH,
            LIFECYCLE_FIXTURE,
            FAILURES_FIXTURE,
        ):
            path.read_bytes().decode("ascii")

    def test_valid_fixture_envelopes_match_schema(self):
        for message in self.messages + self.scenario_messages + self.error_responses:
            self.assertEqual(
                self.validator.errors(message),
                [],
                f"{message.get('type')}: {self.validator.errors(message)}",
            )

        for case in self.failures["invalid_frames"]:
            self.assertNotEqual(self.validator.errors(case["frame"]), [], case["name"])
        for frame in self.failures["schema_invalid_frames"]:
            self.assertNotEqual(self.validator.errors(frame), [], frame["message_id"])
        for response in self.action_recovery:
            self.assertEqual(
                self.validator.errors(
                    response,
                    self.schema["$defs"]["action_status_response"],
                ),
                [],
            )

    def test_schema_covers_every_browser_message(self):
        expected_types = {
            "session.snapshot",
            "state.delta",
            "state.resync",
            "prompt.snapshot",
            "output.subscribe",
            "output.unsubscribe",
            "output.snapshot",
            "action.request",
            "action.received",
            "action.result",
            "protocol.error",
        }
        self.assertEqual(set(self.schema["$defs"]["message_type"]["enum"]), expected_types)

        schema_types = {
            branch["allOf"][1]["properties"]["type"]["const"]
            for branch in self.schema["oneOf"]
        }
        self.assertEqual(schema_types, expected_types)
        observed_types = {
            message["type"]
            for message in self.messages + self.scenario_messages + self.error_responses
        }
        self.assertEqual(observed_types, expected_types)

    def test_all_schema_refs_are_local_and_resolvable(self):
        definitions = self.schema["$defs"]

        def check(node):
            if isinstance(node, dict):
                if "$ref" in node:
                    self.assertTrue(node["$ref"].startswith("#/$defs/"))
                    name = node["$ref"].removeprefix("#/$defs/")
                    self.assertIn(name, definitions)
                for value in node.values():
                    check(value)
            elif isinstance(node, list):
                for value in node:
                    check(value)

        check(self.schema)

    def test_uuidv7_ids_and_utc_timestamps(self):
        messages = self.messages + self.scenario_messages + self.error_responses
        message_ids = set()
        uuid_fields = {
            "message_id",
            "session_id",
            "host_id",
            "action_id",
            "subscription_id",
            "state_epoch",
            "connector_epoch",
            "previous_connector_epoch",
            "expected_epoch",
            "in_reply_to",
        }

        def check_ids(node):
            if isinstance(node, dict):
                for key, value in node.items():
                    if key in uuid_fields and value is not None:
                        parsed = uuid.UUID(value)
                        self.assertEqual(parsed.version, 7, f"{key}: {value}")
                        self.assertEqual(parsed.variant, uuid.RFC_4122, f"{key}: {value}")
                    check_ids(value)
            elif isinstance(node, list):
                for value in node:
                    check_ids(value)

        for message in messages:
            check_ids(message)
            self.assertNotIn(message["message_id"], message_ids)
            message_ids.add(message["message_id"])
            sent_at = self._timestamp(message["sent_at"])
            uuid_timestamp_ms = int(message["message_id"].replace("-", "")[:12], 16)
            self.assertLess(abs(uuid_timestamp_ms - int(sent_at.timestamp() * 1000)), 60_000)

        for snapshot in [
            message for message in messages if message["type"] == "session.snapshot"
        ]:
            self.assertEqual(self._timestamp(snapshot["body"]["server_time"]).utcoffset(), datetime.timedelta(0))

        for response in self.action_recovery:
            parsed = uuid.UUID(response["action_id"])
            self.assertEqual(parsed.version, 7)
            self._timestamp(response["requested_at"])
            self._timestamp(response["completed_at"])

    def test_state_sequences_are_exact_and_resync_replaces_epoch(self):
        epoch = None
        sequence = None
        resync_pending = False
        for message in self.messages:
            body = message["body"]
            if message["type"] == "session.snapshot":
                self.assertEqual(body["sequence"], 0)
                if resync_pending:
                    self.assertNotEqual(body["state_epoch"], epoch)
                epoch = body["state_epoch"]
                sequence = 0
                resync_pending = False
            elif message["type"] == "state.delta":
                self.assertEqual(body["state_epoch"], epoch)
                self.assertEqual(body["sequence"], sequence + 1)
                sequence = body["sequence"]
            elif message["type"] == "state.resync":
                self.assertEqual(body["expected_epoch"], epoch)
                self.assertEqual(body["expected_sequence"], sequence + 1)
                resync_pending = True
        self.assertFalse(resync_pending)

        epoch_change = next(
            change
            for message in self.messages
            if message["type"] == "state.delta"
            for change in message["body"]["changes"]
            if change["operation"] == "instance.epoch_changed"
        )
        self.assertNotEqual(
            epoch_change["previous_connector_epoch"], epoch_change["connector_epoch"]
        )
        resync = next(
            message["body"] for message in self.messages if message["type"] == "state.resync"
        )
        self.assertEqual(resync["reason"], "connector_epoch_changed")

        gap = self._scenario("state_gap_resync")
        delta, resync, snapshot = gap["messages"]
        self.assertEqual(delta["body"]["sequence"], gap["last_accepted_sequence"] + 2)
        self.assertEqual(
            resync["body"]["expected_sequence"],
            gap["last_accepted_sequence"] + 1,
        )
        self.assertNotEqual(
            snapshot["body"]["state_epoch"], delta["body"]["state_epoch"]
        )

    def test_exact_targets_preconditions_and_revisions(self):
        first_snapshot = self.messages[0]["body"]
        host = first_snapshot["hosts"][0]
        instance = host["instances"][0]
        agent = instance["agents"][0]
        self.assertEqual(agent["connector_epoch"], instance["connector_epoch"])
        target = {
            "host_id": host["host_id"],
            "instance_id": instance["instance_id"],
            "terminal_id": agent["terminal_id"],
        }

        bound_types = {"prompt.snapshot", "output.subscribe", "output.snapshot", "action.request"}
        for message in self.messages:
            if message["type"] in bound_types:
                self.assertEqual(message["body"]["target"], target)

        prompt = next(
            message["body"] for message in self.messages if message["type"] == "prompt.snapshot"
        )
        output = next(
            message["body"] for message in self.messages if message["type"] == "output.snapshot"
        )
        for body in (prompt, output):
            self.assertEqual(body["state_epoch"], first_snapshot["state_epoch"])
            self.assertEqual(body["connector_epoch"], instance["connector_epoch"])
            self.assertEqual(body["agent_generation"], agent["agent_generation"])
            self.assertEqual(body["herdr_input_revision"], agent["herdr_input_revision"])

        requests = [
            message["body"] for message in self.messages if message["type"] == "action.request"
        ]
        for request in requests:
            expected = request["expected"]
            self.assertEqual(expected["state_epoch"], first_snapshot["state_epoch"])
            self.assertEqual(expected["connector_epoch"], instance["connector_epoch"])
            self.assertEqual(expected["agent_generation"], agent["agent_generation"])
            self.assertEqual(expected["herdr_input_revision"], agent["herdr_input_revision"])
            self.assertEqual(expected["agent"], agent["agent"])
            self.assertIn(agent["status"], expected["statuses"])

        prompt_request = next(
            request for request in requests if request["operation"]["type"] == "prompt.respond"
        )
        self.assertEqual(prompt_request["expected"]["prompt_fingerprint"], prompt["fingerprint"])
        self.assertEqual(prompt_request["expected"]["herdr_content_hash"], prompt["herdr_content_hash"])

        delta_agent = next(
            message["body"]["changes"][0]["agent"]
            for message in self.messages
            if message["type"] == "state.delta"
        )
        self.assertEqual(delta_agent["agent_generation"], agent["agent_generation"] + 1)
        self.assertEqual(delta_agent["connector_epoch"], instance["connector_epoch"])
        self.assertEqual(delta_agent["herdr_input_revision"], 0)

        read_result = next(
            message["body"]["result"]
            for message in self.messages
            if message["type"] == "action.result"
            and message["body"]["operation_type"] == "agent.read"
        )
        self.assertEqual(read_result["connector_epoch"], instance["connector_epoch"])

    def test_prompt_and_output_hashes_match_exact_content(self):
        prompt = next(
            message["body"] for message in self.messages if message["type"] == "prompt.snapshot"
        )
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
        self.assertEqual(prompt["fingerprint"], self._content_hash(canonical_json))
        self.assertEqual(prompt["herdr_content_hash"], self._content_hash(prompt["excerpt"]))

        content_records = []
        for message in self.messages + self.scenario_messages:
            if message["type"] == "output.snapshot":
                content_records.append(message["body"])
            elif message["type"] == "action.result" and message["body"]["operation_type"] == "agent.read":
                if message["body"]["result"] is not None:
                    content_records.append(message["body"]["result"])
        read_outcome = next(
            case for case in self.failures["operation_outcomes"] if case["type"] == "agent.read"
        )
        content_records.append(read_outcome["success"]["result"])
        for record in content_records:
            self.assertEqual(record["content_revision"], self._content_hash(record["text"]))

    def test_capability_gating_keeps_herdr_073_read_only(self):
        declared_cases = self._semantic_case_names()
        self.assertTrue(
            {
                "prompt_response_without_checked_input",
                "herdr_073_checked_input",
                "herdr_073_nonzero_revision",
            }.issubset(declared_cases)
        )
        outcomes = {case["type"]: case for case in self.failures["operation_outcomes"]}
        self.assertEqual(outcomes["agent.read"]["required_capabilities"], ["read.v1"])
        write_operations = set(outcomes) - {"agent.read"}
        for operation in write_operations:
            self.assertIn("checked_input.v1", outcomes[operation]["required_capabilities"])
        self.assertEqual(
            outcomes["prompt.respond"]["required_capabilities"],
            ["checked_input.v1", "prompt.respond.v1"],
        )

        snapshot_capabilities = self.messages[0]["body"]["hosts"][0]["instances"][0]["capabilities"]
        self.assertNotIn("checked_input.v1", snapshot_capabilities)
        read_only = self._scenario("read_only_write_rejection")
        self.assertEqual(read_only["herdr_version"], "0.7.3")
        self.assertNotIn("checked_input.v1", read_only["advertised_capabilities"])
        self.assertEqual(
            [message["type"] for message in read_only["messages"]],
            ["action.request", "action.result"],
        )
        request, result = [message["body"] for message in read_only["messages"]]
        self.assertEqual(request["expected"]["herdr_input_revision"], 0)
        self.assertEqual(result["status"], "rejected")
        self.assertEqual(result["code"], "HERDR_INCOMPATIBLE")

        checked_073 = copy.deepcopy(self.messages[0])
        checked_073["body"]["hosts"][0]["instances"][0]["capabilities"].append(
            "checked_input.v1"
        )
        self.assertNotEqual(self.validator.errors(checked_073), [])

        revised_073 = copy.deepcopy(self.messages[0])
        revised_073["body"]["hosts"][0]["instances"][0]["agents"][0][
            "herdr_input_revision"
        ] = 1
        self.assertNotEqual(self.validator.errors(revised_073), [])

        checked_fork = copy.deepcopy(self.messages[0])
        fork_instance = checked_fork["body"]["hosts"][0]["instances"][0]
        fork_instance["herdr_version"] = "0.7.3-checked.1"
        fork_instance["capabilities"].append("checked_input.v1")
        fork_instance["agents"][0]["herdr_input_revision"] = 7
        self.assertEqual(self.validator.errors(checked_fork), [])

        revised_prompt_073 = copy.deepcopy(
            next(message for message in self.messages if message["type"] == "prompt.snapshot")
        )
        revised_prompt_073["body"]["herdr_input_revision"] = 1
        self.assertEqual(self.validator.errors(revised_prompt_073), [])
        self.assertNotEqual(
            self._projection_errors([self.messages[0], revised_prompt_073]),
            [],
        )

        unguarded_prompt_response = copy.deepcopy(self.messages[0])
        projected_instance = unguarded_prompt_response["body"]["hosts"][0]["instances"][0]
        projected_instance["herdr_version"] = "1.0.0"
        projected_instance["capabilities"] = ["prompt.respond.v1"]
        self.assertNotEqual(self.validator.errors(unguarded_prompt_response), [])

    def test_every_operation_defines_all_outcomes(self):
        expected_operations = {
            "agent.read",
            "agent.send_text",
            "agent.send_keys",
            "agent.send_input",
            "agent.interrupt",
            "prompt.respond",
        }
        outcomes = self.failures["operation_outcomes"]
        self.assertEqual({case["type"] for case in outcomes}, expected_operations)
        schema_operations = {
            operation["properties"]["type"]["const"]
            for operation in self.schema["$defs"]["operation"]["oneOf"]
        }
        self.assertEqual(schema_operations, expected_operations)

        for case in outcomes:
            self.assertEqual(case["success"]["status"], "succeeded")
            self.assertIsNone(case["success"]["code"])
            self.assertIn(case["pre_execution_failure"]["status"], {"rejected", "failed"})
            if case["type"] == "agent.read":
                self.assertEqual(
                    case["internal_before_enqueue"],
                    {"status": "failed", "code": "INTERNAL"},
                )
            else:
                self.assertEqual(
                    case["internal_before_enqueue"],
                    {"status": "rejected", "code": "INTERNAL"},
                )
            self.assertEqual(
                case["timeout_before_receipt"],
                {"status": "rejected", "code": "DEADLINE_EXCEEDED"},
            )
            if case["type"] == "agent.read":
                self.assertEqual(
                    case["timeout_after_receipt"],
                    {"status": "failed", "code": "DEADLINE_EXCEEDED"},
                )
                self.assertEqual(
                    case["disconnect_after_receipt"],
                    {"status": "failed", "code": "CONNECTION_LOST"},
                )
                self.assertEqual(
                    case["browser_disconnect_unresolved"],
                    {"status": "failed", "code": "CONNECTION_LOST"},
                )
            else:
                expected_unknown = {"status": "unknown", "code": "OUTCOME_UNKNOWN"}
                self.assertEqual(case["timeout_after_receipt"], expected_unknown)
                self.assertEqual(case["disconnect_after_receipt"], expected_unknown)
                self.assertEqual(case["browser_disconnect_unresolved"], expected_unknown)

    def test_failure_lifecycles_distinguish_receipt_boundary(self):
        before = self._scenario("timeout_before_receipt")["messages"]
        self.assertNotIn("action.received", [message["type"] for message in before])
        self.assertEqual(before[-1]["body"]["status"], "rejected")

        after = self._scenario("timeout_after_receipt")["messages"]
        self.assertEqual(
            [message["type"] for message in after],
            ["action.request", "action.received", "action.result"],
        )
        self.assertEqual(after[-1]["body"]["status"], "unknown")

        disconnected = self._scenario("disconnect_unknown_write")["messages"]
        self.assertEqual(disconnected[-1]["body"]["status"], "unknown")
        self.assertEqual(disconnected[-1]["body"]["code"], "OUTCOME_UNKNOWN")

        stale = self._scenario("stale_state")
        stale_request = stale["messages"][0]["body"]["expected"]
        self.assertNotEqual(stale_request["agent_generation"], stale["current_agent_generation"])
        self.assertNotEqual(
            stale_request["herdr_input_revision"], stale["current_herdr_input_revision"]
        )
        self.assertEqual(stale["messages"][-1]["body"]["code"], "STALE_STATE")

    def test_duplicate_action_ids_are_rejected_across_sessions(self):
        same_session = self._scenario("same_session_duplicate_action")
        cross_session = self._scenario("cross_session_duplicate_action")
        for scenario in (same_session, cross_session):
            request, result = scenario["messages"]
            self.assertEqual(
                request["body"]["action_id"], scenario["previously_audited_action_id"]
            )
            self.assertEqual(result["body"]["action_id"], request["body"]["action_id"])
            self.assertEqual(result["body"]["status"], "rejected")
            self.assertEqual(result["body"]["code"], "DUPLICATE_ACTION")
            self.assertNotIn(
                "action.received", [message["type"] for message in scenario["messages"]]
            )
        self.assertNotEqual(
            same_session["messages"][0]["body"]["session_id"],
            cross_session["messages"][0]["body"]["session_id"],
        )

    def test_unresolved_actions_use_metadata_only_recovery_without_replay(self):
        recoveries = {case["name"]: case for case in self.failures["action_recovery"]}
        for name in ("unresolved_write_before_receipt", "unresolved_write_after_receipt"):
            response = recoveries[name]["response"]
            self.assertEqual(response["status"], "unknown")
            self.assertEqual(response["code"], "OUTCOME_UNKNOWN")
            self.assertNotEqual(
                recoveries[name]["receipt_observed"],
                recoveries[
                    "unresolved_write_after_receipt"
                    if name == "unresolved_write_before_receipt"
                    else "unresolved_write_before_receipt"
                ]["receipt_observed"],
            )
        read = recoveries["unresolved_read"]["response"]
        self.assertEqual(read["status"], "failed")
        self.assertEqual(read["code"], "CONNECTION_LOST")
        for response in self.action_recovery:
            self.assertTrue(
                {"text", "keys", "result", "message", "prompt", "output"}.isdisjoint(
                    response
                )
            )

    def test_action_result_status_code_combinations_are_closed(self):
        for frame in self.failures["schema_invalid_frames"]:
            self.assertNotEqual(self.validator.errors(frame), [])

        valid_results = [
            message
            for message in self.messages + self.scenario_messages
            if message["type"] == "action.result"
        ]
        for result in valid_results:
            self.assertEqual(self.validator.errors(result), [])

        rejected_codes = set(self.schema["$defs"]["rejected_code"]["enum"])
        self.assertIn("STALE_STATE", rejected_codes)
        self.assertIn("DUPLICATE_ACTION", rejected_codes)
        self.assertNotIn("OUTCOME_UNKNOWN", rejected_codes)
        self.assertNotIn("CONNECTION_LOST", rejected_codes)

        write_internal = copy.deepcopy(
            self._scenario("stale_state")["messages"][-1]
        )
        write_internal["body"]["code"] = "INTERNAL"
        self.assertEqual(self.validator.errors(write_internal), [])

        read_internal = copy.deepcopy(write_internal)
        read_internal["body"]["operation_type"] = "agent.read"
        read_internal["body"]["status"] = "failed"
        self.assertEqual(self.validator.errors(read_internal), [])

        definitive_write_failure = copy.deepcopy(write_internal)
        definitive_write_failure["body"]["status"] = "failed"
        definitive_write_failure["body"]["code"] = "HERDR_REJECTED"
        self.assertEqual(self.validator.errors(definitive_write_failure), [])

        invalid_failed_internal_write = copy.deepcopy(write_internal)
        invalid_failed_internal_write["body"]["status"] = "failed"
        self.assertNotEqual(self.validator.errors(invalid_failed_internal_write), [])

        recovered_write_internal = copy.deepcopy(self.action_recovery[0])
        recovered_write_internal["status"] = "rejected"
        recovered_write_internal["code"] = "INTERNAL"
        self.assertEqual(
            self.validator.errors(
                recovered_write_internal,
                self.schema["$defs"]["action_status_response"],
            ),
            [],
        )

    def test_errors_and_failure_metadata_have_no_terminal_content(self):
        forbidden_keys = {
            "excerpt",
            "keys",
            "message",
            "output",
            "prompt",
            "sent_text",
            "text",
        }
        error_bodies = [
            message["body"]
            for message in self.messages + self.scenario_messages + self.error_responses
            if message["type"] == "protocol.error"
            or (message["type"] == "action.result" and message["body"]["status"] != "succeeded")
        ]
        for body in error_bodies:
            self.assertTrue(forbidden_keys.isdisjoint(body))
            serialized = json.dumps(body, sort_keys=True)
            self.assertNotIn("Permission required", serialized)
            self.assertNotIn("please continue", serialized)

        protocol_error_properties = set(
            self.schema["$defs"]["protocol_error_body"]["properties"]
        )
        action_result_properties = set(
            self.schema["$defs"]["action_result_body"]["properties"]
        )
        self.assertTrue(forbidden_keys.isdisjoint(protocol_error_properties))
        self.assertTrue(forbidden_keys.isdisjoint(action_result_properties))

    def test_schema_exposes_no_connector_routes_credentials_or_extra_operations(self):
        serialized = json.dumps(self.schema, sort_keys=True)
        for forbidden in (
            '"pane_id"',
            '"tab_id"',
            '"workspace_id"',
            '"credential"',
            '"shared_token"',
        ):
            self.assertNotIn(forbidden, serialized)

        operations = {
            operation["properties"]["type"]["const"]
            for operation in self.schema["$defs"]["operation"]["oneOf"]
        }
        self.assertFalse(
            operations
            & {
                "agent.launch",
                "agent.terminate",
                "host.shutdown",
                "host.wake",
                "telegram.send",
            }
        )

    def test_logical_identity_and_prompt_option_uniqueness(self):
        self.assertTrue(
            {
                "duplicate_host_id",
                "duplicate_instance_id",
                "duplicate_terminal_id",
                "duplicate_prompt_option_id",
            }.issubset(self._semantic_case_names())
        )
        for message in self.messages:
            self.assertEqual(self._logical_uniqueness_errors(message), [])

        duplicate_host = copy.deepcopy(self.messages[0])
        host = copy.deepcopy(duplicate_host["body"]["hosts"][0])
        host["display_name"] = "duplicate workstation"
        duplicate_host["body"]["hosts"].append(host)

        duplicate_instance = copy.deepcopy(self.messages[0])
        instance = copy.deepcopy(
            duplicate_instance["body"]["hosts"][0]["instances"][0]
        )
        instance["status"] = "degraded"
        duplicate_instance["body"]["hosts"][0]["instances"].append(instance)

        duplicate_agent = copy.deepcopy(self.messages[0])
        agent = copy.deepcopy(
            duplicate_agent["body"]["hosts"][0]["instances"][0]["agents"][0]
        )
        agent["status"] = "working"
        duplicate_agent["body"]["hosts"][0]["instances"][0]["agents"].append(agent)

        duplicate_option = copy.deepcopy(
            next(message for message in self.messages if message["type"] == "prompt.snapshot")
        )
        option = copy.deepcopy(duplicate_option["body"]["options"][0])
        option["label"] = "Same ID, different label"
        duplicate_option["body"]["options"].append(option)

        for invalid in (
            duplicate_host,
            duplicate_instance,
            duplicate_agent,
            duplicate_option,
        ):
            self.assertEqual(self.validator.errors(invalid), [])
            self.assertNotEqual(self._logical_uniqueness_errors(invalid), [])

    def test_connector_generations_are_scoped_and_never_regress(self):
        self.assertIn("connector_generation_regression", self._semantic_case_names())
        self.assertIn("connector_epoch_change_without_resync", self._semantic_case_names())
        self.assertEqual(self._projection_errors(self.messages), [])
        initial = self.messages[0]
        first_delta = next(
            message
            for message in self.messages
            if message["type"] == "state.delta"
            and message["body"]["changes"][0]["operation"] == "agent.upsert"
        )
        regression = copy.deepcopy(first_delta)
        regression["message_id"] = "019f64ca-3000-7000-8000-000000000150"
        regression["body"]["sequence"] = 2
        regression["body"]["changes"][0]["agent"]["agent_generation"] = 1
        self.assertNotEqual(
            self._projection_errors([initial, first_delta, regression]),
            [],
        )

        silent_epoch_change = copy.deepcopy(first_delta)
        silent_epoch_change["message_id"] = "019f64ca-3000-7000-8000-000000000151"
        silent_epoch_change["body"]["sequence"] = 1
        silent_epoch_change["body"]["changes"] = [
            {
                "operation": "instance.upsert",
                "host_id": "019f64ca-1000-7000-8000-000000000002",
                "instance_id": "default",
                "instance": {
                    "connector_epoch": "019f64ca-3000-7000-8000-000000000111",
                    "herdr_version": "0.7.3",
                    "herdr_protocol": 16,
                    "status": "online",
                    "capabilities": ["read.v1"],
                },
            }
        ]
        self.assertEqual(self.validator.errors(silent_epoch_change), [])
        self.assertNotEqual(
            self._projection_errors([initial, silent_epoch_change]),
            [],
        )

        snapshots = [
            message["body"] for message in self.messages if message["type"] == "session.snapshot"
        ]
        old_instance = snapshots[0]["hosts"][0]["instances"][0]
        new_instance = snapshots[1]["hosts"][0]["instances"][0]
        self.assertNotEqual(old_instance["connector_epoch"], new_instance["connector_epoch"])
        self.assertEqual(new_instance["agents"][0]["agent_generation"], 1)

    def test_standard_code_point_limits_guarantee_utf8_byte_ceilings(self):
        serialized = json.dumps(self.schema, sort_keys=True)
        self.assertNotIn("x-maxUtf8Bytes", serialized)
        limits = {
            "input_text": 16384,
            "read_result": 131072,
            "output_snapshot_body": 131072,
            "prompt_snapshot_body": 8192,
        }
        definitions = self.schema["$defs"]
        maxima = {
            "input_text": definitions["input_text"]["maxLength"],
            "read_result": definitions["read_result"]["properties"]["text"]["maxLength"],
            "output_snapshot_body": definitions["output_snapshot_body"]["properties"]["text"]["maxLength"],
            "prompt_snapshot_body": definitions["prompt_snapshot_body"]["properties"]["excerpt"]["maxLength"],
        }
        for name, byte_limit in limits.items():
            self.assertLessEqual(maxima[name] * 4, byte_limit)

    def test_prompt_projection_truncates_display_without_rehashing(self):
        prompt_message = next(
            message for message in self.messages if message["type"] == "prompt.snapshot"
        )
        connector_excerpt = "x" * 2049
        connector_body = copy.deepcopy(prompt_message["body"])
        connector_body["excerpt"] = connector_excerpt
        connector_body["excerpt_truncated"] = False
        connector_body["fingerprint"] = self._content_hash("connector canonical prompt")
        connector_body["herdr_content_hash"] = self._content_hash(connector_excerpt)

        projected = self._project_prompt(connector_body)
        projected_message = copy.deepcopy(prompt_message)
        projected_message["body"] = projected
        self.assertEqual(self.validator.errors(projected_message), [])
        self.assertEqual(len(projected["excerpt"]), 2048)
        self.assertTrue(projected["excerpt_truncated"])
        self.assertEqual(projected["fingerprint"], connector_body["fingerprint"])
        self.assertEqual(
            projected["herdr_content_hash"], connector_body["herdr_content_hash"]
        )
        self.assertNotEqual(
            projected["herdr_content_hash"], self._content_hash(projected["excerpt"])
        )

        unprojected = copy.deepcopy(prompt_message)
        unprojected["body"] = connector_body
        self.assertNotEqual(self.validator.errors(unprojected), [])

        connector_already_truncated = copy.deepcopy(connector_body)
        connector_already_truncated["excerpt"] = "y" * 2048
        connector_already_truncated["excerpt_truncated"] = True
        projected_already_truncated = self._project_prompt(connector_already_truncated)
        self.assertTrue(projected_already_truncated["excerpt_truncated"])
        self.assertEqual(
            projected_already_truncated["fingerprint"],
            connector_already_truncated["fingerprint"],
        )

        unchanged = self._project_prompt(prompt_message["body"])
        self.assertFalse(unchanged["excerpt_truncated"])
        self.assertEqual(unchanged["excerpt"], prompt_message["body"]["excerpt"])

    @staticmethod
    def _timestamp(value):
        parsed = datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
        if parsed.utcoffset() != datetime.timedelta(0):
            raise AssertionError(f"timestamp is not UTC: {value}")
        return parsed

    @staticmethod
    def _content_hash(value):
        return "sha256:" + hashlib.sha256(value.encode()).hexdigest()

    def _scenario(self, name):
        return next(
            scenario for scenario in self.failures["scenarios"] if scenario["name"] == name
        )

    @staticmethod
    def _project_prompt(connector_body):
        projected = copy.deepcopy(connector_body)
        browser_truncated = len(projected["excerpt"]) > 2048
        projected["excerpt"] = projected["excerpt"][:2048]
        projected["excerpt_truncated"] = (
            projected["excerpt_truncated"] or browser_truncated
        )
        return projected

    def _semantic_case_names(self):
        return {case["name"] for case in self.failures["semantic_invalid_cases"]}

    @staticmethod
    def _logical_uniqueness_errors(message):
        errors = []

        def check_unique(records, key, label):
            values = [record[key] for record in records]
            if len(values) != len(set(values)):
                errors.append(f"duplicate {label}")

        if message["type"] == "session.snapshot":
            hosts = message["body"]["hosts"]
            check_unique(hosts, "host_id", "host_id")
            for host in hosts:
                check_unique(host["instances"], "instance_id", "instance_id")
                for instance in host["instances"]:
                    check_unique(instance["agents"], "terminal_id", "terminal_id")
                    for agent in instance["agents"]:
                        if agent["connector_epoch"] != instance["connector_epoch"]:
                            errors.append("agent connector_epoch differs from instance")
        elif message["type"] == "prompt.snapshot":
            check_unique(message["body"]["options"], "id", "prompt option id")
        return errors

    @staticmethod
    def _projection_errors(messages):
        errors = []
        instances = {}
        generations = {}
        pending_epoch = None
        awaiting_snapshot = None

        for message in messages:
            body = message["body"]
            if pending_epoch is not None:
                if message["type"] != "state.resync" or body["reason"] != "connector_epoch_changed":
                    errors.append("connector epoch change did not force state.resync")
                else:
                    awaiting_snapshot = pending_epoch
                pending_epoch = None
                continue
            if awaiting_snapshot is not None:
                if message["type"] != "session.snapshot":
                    errors.append("connector epoch resync did not produce a snapshot")
                    continue

            if message["type"] == "session.snapshot":
                instances = {}
                generations = {}
                for host in body["hosts"]:
                    for instance in host["instances"]:
                        instance_key = (host["host_id"], instance["instance_id"])
                        instances[instance_key] = {
                            "connector_epoch": instance["connector_epoch"],
                            "herdr_version": instance["herdr_version"],
                            "capabilities": instance["capabilities"],
                        }
                        for agent in instance["agents"]:
                            key = instance_key + (
                                agent["connector_epoch"],
                                agent["terminal_id"],
                            )
                            generations[key] = agent["agent_generation"]
                if awaiting_snapshot is not None:
                    expected_key, expected_epoch = awaiting_snapshot
                    if (
                        instances.get(expected_key, {}).get("connector_epoch")
                        != expected_epoch
                    ):
                        errors.append("snapshot did not install changed connector epoch")
                    awaiting_snapshot = None
            elif message["type"] == "state.delta":
                for change in body["changes"]:
                    if change["operation"] == "agent.upsert":
                        target = change["target"]
                        agent = change["agent"]
                        instance_key = (target["host_id"], target["instance_id"])
                        if (
                            instances.get(instance_key, {}).get("connector_epoch")
                            != agent["connector_epoch"]
                        ):
                            errors.append("agent delta uses a different connector epoch")
                        key = instance_key + (
                            agent["connector_epoch"],
                            target["terminal_id"],
                        )
                        previous = generations.get(key)
                        if previous is not None and agent["agent_generation"] < previous:
                            errors.append("connector generation regressed within one epoch")
                        if (
                            instances.get(instance_key, {}).get("herdr_version") == "0.7.3"
                            and agent["herdr_input_revision"] != 0
                        ):
                            errors.append("Herdr 0.7.3 projected a usable write revision")
                        generations[key] = agent["agent_generation"]
                    elif change["operation"] == "instance.upsert":
                        instance_key = (change["host_id"], change["instance_id"])
                        projected = instances.get(instance_key)
                        replacement = change["instance"]
                        if (
                            projected is not None
                            and projected["connector_epoch"]
                            != replacement["connector_epoch"]
                        ):
                            errors.append("instance upsert silently changed connector epoch")
                        else:
                            instances[instance_key] = {
                                "connector_epoch": replacement["connector_epoch"],
                                "herdr_version": replacement["herdr_version"],
                                "capabilities": replacement["capabilities"],
                            }
                    elif change["operation"] == "instance.epoch_changed":
                        instance_key = (change["host_id"], change["instance_id"])
                        if (
                            instances.get(instance_key, {}).get("connector_epoch")
                            != change["previous_connector_epoch"]
                        ):
                            errors.append("connector epoch transition has the wrong predecessor")
                        if change["connector_epoch"] == change["previous_connector_epoch"]:
                            errors.append("connector epoch did not change")
                        pending_epoch = (instance_key, change["connector_epoch"])
            elif message["type"] in {
                "prompt.snapshot",
                "output.snapshot",
                "action.request",
            }:
                target = body["target"]
                instance_key = (target["host_id"], target["instance_id"])
                projected = instances.get(instance_key)
                if projected is None:
                    errors.append("record refers to an unknown instance")
                    continue
                state = body["expected"] if message["type"] == "action.request" else body
                if state["connector_epoch"] != projected["connector_epoch"]:
                    errors.append("record uses a stale connector epoch")
                if (
                    projected["herdr_version"] == "0.7.3"
                    and state["herdr_input_revision"] != 0
                ):
                    errors.append("Herdr 0.7.3 projected a usable write revision")

        if pending_epoch is not None or awaiting_snapshot is not None:
            errors.append("connector epoch transition is incomplete")
        return errors


if __name__ == "__main__":
    unittest.main()
