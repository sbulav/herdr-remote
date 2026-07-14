import asyncio
import json
import subprocess
import threading
import unittest
from unittest.mock import patch

from relay import herdr_relay


class HostStatusTests(unittest.TestCase):
    @patch.object(herdr_relay, "HOST_TARGETS", {"mba13": "mba", "mz": "mz"})
    @patch.object(herdr_relay, "run_herdr_checked")
    def test_host_status_reflects_poll_success(self, run_herdr_checked):
        empty_result = json.dumps({"result": {"panes": []}})
        run_herdr_checked.side_effect = [
            (False, ""),
            (True, empty_result),
        ]

        agents, hosts = asyncio.run(herdr_relay.get_all_agents())

        self.assertEqual(agents, [])
        self.assertEqual(
            hosts,
            [
                {"host_id": "mba13", "online": False},
                {"host_id": "mz", "online": True},
            ],
        )

    @patch.object(herdr_relay, "HOST_TARGETS", {"host-a": "a", "host-b": "b"})
    @patch.object(herdr_relay, "get_agents_from_host")
    def test_hosts_are_polled_concurrently(self, get_agents_from_host):
        barrier = threading.Barrier(2, timeout=1)

        def poll_host(*, remote, host_id):
            barrier.wait()
            return ([{"pane_id": remote}], True)

        get_agents_from_host.side_effect = poll_host

        agents, hosts = asyncio.run(herdr_relay.get_all_agents())

        self.assertEqual(agents, [{"pane_id": "a"}, {"pane_id": "b"}])
        self.assertEqual(
            hosts,
            [
                {"host_id": "host-a", "online": True},
                {"host_id": "host-b", "online": True},
            ],
        )

    @patch.object(herdr_relay.subprocess, "run")
    def test_remote_poll_uses_keepalives(self, run):
        run.return_value.returncode = 0
        run.return_value.stdout = "ok\n"

        self.assertEqual(
            herdr_relay.run_herdr_checked("pane", "list", remote="workstation"),
            (True, "ok"),
        )
        run.assert_called_once_with(
            [
                "ssh",
                "-o", "ConnectTimeout=5",
                "-o", "ServerAliveInterval=3",
                "-o", "ServerAliveCountMax=2",
                "-o", "BatchMode=yes",
                "workstation", herdr_relay.HERDR, "pane", "list",
            ],
            capture_output=True,
            text=True,
            timeout=15,
        )

    @patch.object(herdr_relay.subprocess, "run")
    def test_remote_poll_reports_failures(self, run):
        run.side_effect = subprocess.TimeoutExpired("ssh", 15)

        with patch("builtins.print") as print_message:
            result = herdr_relay.run_herdr_checked(
                "pane", "list", remote="workstation"
            )

        self.assertEqual(result, (False, ""))
        print_message.assert_called_once()
        self.assertIn("workstation", print_message.call_args.args[0])


class PaneChromeTests(unittest.TestCase):
    @patch.object(herdr_relay, "run_herdr")
    def test_read_pane_filters_heavy_opencode_chrome(self, run_herdr):
        run_herdr.return_value = "\n".join(
            [
                "┃ Permission required: access external directory ┃",
                "╹▀▀▀▀▀▀▀▀",
                "⬝⬝⬝⬝ esc interrupt",
            ]
        )

        self.assertEqual(
            herdr_relay.read_pane("pane-1"),
            "┃ Permission required: access external directory ┃",
        )

    def test_meaningful_status_with_footer_is_not_all_chrome(self):
        line = "┃ ┃ Build · GPT-5.6 Sol OpenAI ~/src:main ╹▀▀ ⬝⬝ esc interrupt"

        self.assertIsNone(herdr_relay.CHROME_RE.search(line))


class StructuredOutputTests(unittest.TestCase):
    def test_claude_project_dir(self):
        self.assertEqual(
            herdr_relay.claude_project_dir("/Users/me/src/herdr-mobile"),
            "-Users-me-src-herdr-mobile",
        )
        self.assertEqual(
            herdr_relay.claude_project_dir("/home/me/my_app.v2"),
            "-home-me-my-app-v2",
        )

    def test_summarize_tool(self):
        self.assertEqual(
            herdr_relay.summarize_tool({"file_path": "/etc/hosts", "content": "x"}),
            "/etc/hosts",
        )
        self.assertEqual(
            herdr_relay.summarize_tool({"command": "make build\nmake test"}),
            "make build",
        )
        self.assertEqual(herdr_relay.summarize_tool(None), "")

    def test_claude_transcript_mapping(self):
        fixture = "\n".join(
            json.dumps(record)
            for record in [
                {
                    "type": "user",
                    "message": {"role": "user", "content": "Fix the login bug"},
                },
                {
                    "type": "assistant",
                    "message": {
                        "role": "assistant",
                        "content": [
                            {
                                "type": "thinking",
                                "thinking": "Inspect auth\nthen patch",
                            },
                            {"type": "text", "text": "I'll inspect it."},
                            {
                                "type": "tool_use",
                                "name": "Read",
                                "input": {"file_path": "auth.py"},
                            },
                        ],
                    },
                },
            ]
        )

        blocks = herdr_relay.transcript_to_blocks(fixture)

        self.assertEqual(
            [(block["kind"], block.get("label")) for block in blocks],
            [
                ("status", "You"),
                ("status", "Thought"),
                ("assistant_text", None),
                ("tool", "Read"),
            ],
        )
        self.assertEqual(blocks[1]["text"], "Inspect auth")
        self.assertEqual(blocks[2]["markdown"], "I'll inspect it.")
        self.assertEqual(blocks[3]["text"], "auth.py")

    def test_claude_transcript_tolerates_partial_tail(self):
        fixture = "partial-json\n" + json.dumps(
            {
                "type": "assistant",
                "message": {
                    "content": [{"type": "text", "text": "ok"}],
                },
            }
        )
        self.assertEqual(
            [block["kind"] for block in herdr_relay.transcript_to_blocks(fixture)],
            ["assistant_text"],
        )

    def test_claude_transcript_keeps_tail_limit(self):
        fixture = "\n".join(
            json.dumps(
                {
                    "type": "assistant",
                    "message": {
                        "content": [{"type": "text", "text": str(index)}],
                    },
                }
            )
            for index in range(250)
        )
        blocks = herdr_relay.transcript_to_blocks(fixture, limit=10)
        self.assertEqual(
            [block["markdown"] for block in blocks],
            [str(index) for index in range(240, 250)],
        )

    def test_ambiguous_cwd_is_not_streamed(self):
        herdr_relay.pane_cwd_map["ambiguous"] = (
            "/work/repo",
            "claude",
            None,
            True,
        )
        try:
            self.assertEqual(
                herdr_relay.pane_blocks("ambiguous"),
                (None, None),
            )
        finally:
            herdr_relay.pane_cwd_map.pop("ambiguous")

    def test_opencode_mapping(self):
        document = {
            "session_id": "ses_test",
            "updated": 1,
            "rows": [
                ["user", json.dumps({"type": "text", "text": "Fix the login bug"})],
                [
                    "assistant",
                    json.dumps(
                        {"type": "reasoning", "text": "Inspect auth\nthen patch"}
                    ),
                ],
                ["assistant", json.dumps({"type": "text", "text": "I'll inspect it."})],
                [
                    "assistant",
                    json.dumps(
                        {
                            "type": "tool",
                            "tool": "read",
                            "state": {
                                "status": "completed",
                                "input": {"filePath": "auth.py"},
                            },
                        }
                    ),
                ],
                ["assistant", "not-json"],
            ],
        }

        blocks = herdr_relay.opencode_to_blocks(document)

        self.assertEqual(
            [(block["kind"], block.get("label")) for block in blocks],
            [
                ("status", "You"),
                ("status", "Thought"),
                ("assistant_text", None),
                ("tool", "read"),
            ],
        )
        self.assertEqual(blocks[1]["text"], "Inspect auth")
        self.assertEqual(blocks[2]["markdown"], "I'll inspect it.")
        self.assertEqual(blocks[3]["text"], "auth.py")

    def test_opencode_mapping_preserves_multiline_markdown(self):
        markdown = (
            "# Todos\n"
            "[•] Identify degraded state\n"
            "[ ] Correlate agent logs\n\n"
            "$ kubectl get nodes\n"
            "NODE STATE\n"
            "km1 Degraded"
        )
        document = {
            "rows": [
                ["assistant", json.dumps({"type": "text", "text": markdown})],
            ],
        }

        blocks = herdr_relay.opencode_to_blocks(document)

        self.assertEqual(blocks[0]["markdown"], markdown)


class DetectOptionsTests(unittest.TestCase):
    def test_legacy_tool_permission(self):
        text = (
            "Do you want to allow this tool call?\n\n"
            "> yes, single permission\n"
            "> trust, always allow\n"
            "> no (tab to edit)"
        )
        self.assertEqual(herdr_relay.detect_options(text), herdr_relay.TOOL_OPTIONS)

    def test_subagent_options(self):
        text = "approve all pending\nconfigure individually"
        self.assertEqual(herdr_relay.detect_options(text), herdr_relay.SUBAGENT_OPTIONS)

    def test_claude_numbered_yes_no(self):
        text = (
            "Ask rule Bash(git add *) overrides auto mode for this command.\n"
            " /permissions to let auto mode decide\n\n"
            " Do you want to proceed?\n"
            " ❯ 1. Yes\n"
            "   2. No\n"
        )
        self.assertEqual(herdr_relay.detect_options(text), ["1. Yes", "2. No"])

    def test_claude_proceed_fallback_without_numbers(self):
        text = "Do you want to proceed?\nSome other chrome"
        self.assertEqual(herdr_relay.detect_options(text), ["1. Yes", "2. No"])

    def test_claude_ask_rule_fallback(self):
        text = "Ask rule Bash(git add *) overrides auto mode for this command."
        self.assertEqual(herdr_relay.detect_options(text), ["1. Yes", "2. No"])

    def test_opencode_permission_required(self):
        text = (
            "△ Permission required\n"
            "  Bash · git status\n"
            "  Allow once   Allow always   Reject\n"
            "  ↔ select   enter confirm   esc dismiss\n"
        )
        self.assertEqual(herdr_relay.detect_options(text), herdr_relay.OPENCODE_OPTIONS)

    def test_opencode_allow_once_phrase(self):
        text = "Allow once\nAllow always\nReject\nPermission required"
        self.assertEqual(herdr_relay.detect_options(text), herdr_relay.OPENCODE_OPTIONS)

    def test_yn_style(self):
        self.assertEqual(herdr_relay.detect_options("Continue? [y/n]"), ["y", "n"])
        self.assertEqual(herdr_relay.detect_options("write to this file?\nproceed (y)"), ["y", "n"])

    def test_respond_text_numbered_label(self):
        self.assertEqual(herdr_relay.respond_text("1. Yes"), "1")
        self.assertEqual(herdr_relay.respond_text("2. No"), "2")
        self.assertEqual(
            herdr_relay.respond_text("yes, single permission"),
            "yes, single permission",
        )

    def test_respond_action_opencode_keys(self):
        self.assertEqual(herdr_relay.respond_action("Allow once"), ("keys", ["Enter"]))
        self.assertEqual(
            herdr_relay.respond_action("Allow always"),
            ("keys", ["Right", "Enter", "Enter"]),
        )
        self.assertEqual(herdr_relay.respond_action("Reject"), ("keys", ["Escape"]))
        self.assertEqual(herdr_relay.respond_action("1. Yes"), ("text", "1"))
        self.assertEqual(herdr_relay.respond_action("y"), ("text", "y"))
        # Free-text deny must not be remapped to Escape keys
        self.assertEqual(herdr_relay.respond_action("deny"), ("text", "deny"))

    def test_unknown_prompt_returns_none(self):
        self.assertIsNone(herdr_relay.detect_options("just some log output"))


if __name__ == "__main__":
    unittest.main()
