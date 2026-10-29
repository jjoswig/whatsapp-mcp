import asyncio
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch

from mcp import ClientSession, StdioServerParameters
from mcp.client.stdio import stdio_client

import main
import whatsapp
from read_only import READ_ONLY_MESSAGE, parse_read_only


READ_TOOLS = {
    "search_contacts", "list_messages", "list_chats", "get_chat",
    "get_direct_chat_by_contact", "get_contact_chats", "get_last_interaction",
    "get_message_context", "download_media",
}
WRITE_TOOLS = {"send_message", "send_file", "send_audio_message"}
SERVER_DIR = Path(__file__).resolve().parent


class ConfigurationTests(unittest.TestCase):
    def test_boolean_values(self):
        for value in ("true", "1", "yes", "on", " TRUE ", "Yes"):
            with self.subTest(value=value):
                self.assertTrue(parse_read_only(value))
        for value in (None, "false", "0", "no", "off", " FALSE ", "No"):
            with self.subTest(value=value):
                self.assertFalse(parse_read_only(value))

    def test_invalid_values_prevent_startup(self):
        for value in ("", "tru", "enabled", "2"):
            with self.subTest(value=value):
                env = {**os.environ, "WHATSAPP_READ_ONLY": value}
                result = subprocess.run(
                    [sys.executable, str(SERVER_DIR / "main.py")],
                    env=env, capture_output=True, text=True, timeout=15,
                )
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("invalid WHATSAPP_READ_ONLY", result.stderr)

    def test_policy_is_loaded_once_at_startup(self):
        result = subprocess.run(
            [sys.executable, "-c", (
                "import os, read_only, whatsapp, main; "
                "os.environ['WHATSAPP_READ_ONLY'] = 'false'; "
                "assert read_only.READ_ONLY and whatsapp.READ_ONLY; "
                "assert main.send_message('123', 'test')['success'] is False"
            )], cwd=SERVER_DIR,
            env={**os.environ, "WHATSAPP_READ_ONLY": "true"},
            capture_output=True, text=True, timeout=15,
        )
        self.assertEqual(result.returncode, 0, result.stderr)


class ToolPolicyTests(unittest.IsolatedAsyncioTestCase):
    def make_server(self, read_only):
        server = main.WhatsAppMCP(read_only=read_only)
        for name in READ_TOOLS | WRITE_TOOLS:
            server.add_tool(getattr(main, name))
        return server

    async def test_read_only_discovery_and_stale_write_calls(self):
        server = self.make_server(True)
        self.assertEqual({tool.name for tool in await server.list_tools()}, READ_TOOLS)
        for name in WRITE_TOOLS | {"reply", "archive_chat", "remove_participant", "delete_message"}:
            with self.subTest(tool=name):
                with self.assertRaisesRegex(ValueError, "writes are disabled"):
                    await server.call_tool(name, {"recipient": "123", "message": "test"})

    async def test_future_tools_are_denied_by_default(self):
        server = self.make_server(True)
        invoked = Mock()

        @server.tool()
        def archive_chat(chat_jid: str) -> str:
            invoked()
            return "archived"

        server.add_tool(archive_chat, name="another_future_write")
        names = {tool.name for tool in await server.list_tools()}
        self.assertNotIn("archive_chat", names)
        self.assertNotIn("another_future_write", names)
        for name in ("archive_chat", "another_future_write"):
            with self.assertRaisesRegex(ValueError, "writes are disabled"):
                await server.call_tool(name, {"chat_jid": "123@g.us"})
        invoked.assert_not_called()

    async def test_writable_mode_preserves_tool_registration_and_execution(self):
        server = self.make_server(False)
        self.assertEqual({tool.name for tool in await server.list_tools()}, READ_TOOLS | WRITE_TOOLS)
        with patch.object(main, "whatsapp_send_message", return_value=(True, "sent")) as send:
            result = await server.call_tool("send_message", {"recipient": "123", "message": "test"})
        send.assert_called_once_with("123", "test")
        self.assertEqual(json.loads(result[0].text), {"success": True, "message": "sent"})

    async def test_reads_still_query_local_database(self):
        server = self.make_server(True)
        with tempfile.TemporaryDirectory() as directory:
            db_path = str(Path(directory) / "messages.db")
            with sqlite3.connect(db_path) as conn:
                conn.execute("CREATE TABLE chats (jid TEXT, name TEXT)")
                conn.execute("INSERT INTO chats VALUES (?, ?)", ("123@s.whatsapp.net", "Alice"))
            with patch.object(whatsapp, "MESSAGES_DB_PATH", db_path):
                result = await server.call_tool("search_contacts", {"query": "Alice"})
            self.assertIn("Alice", result[0].text)
            self.assertIn("123@s.whatsapp.net", result[0].text)

    async def test_actual_stdio_mcp_session_rejects_writes(self):
        parameters = StdioServerParameters(
            command=sys.executable, args=[str(SERVER_DIR / "main.py")],
            env={**os.environ, "WHATSAPP_READ_ONLY": "true"},
        )
        async with asyncio.timeout(20):
            async with stdio_client(parameters) as (read, write):
                async with ClientSession(read, write) as session:
                    await session.initialize()
                    tools = await session.list_tools()
                    self.assertEqual({tool.name for tool in tools.tools}, READ_TOOLS)
                    for name in WRITE_TOOLS:
                        result = await session.call_tool(name, {"recipient": "123", "message": "test"})
                        self.assertTrue(result.isError)
                        self.assertIn("writes are disabled", result.content[0].text)


class PythonHelperTests(unittest.TestCase):
    def test_every_write_stops_before_requests_files_or_conversion(self):
        with (
            patch.object(whatsapp, "READ_ONLY", True),
            patch.object(whatsapp.requests, "post") as post,
            patch.object(whatsapp.os.path, "isfile") as isfile,
            patch.object(whatsapp.audio, "convert_to_opus_ogg_temp") as convert,
        ):
            for name, second_argument in (
                ("send_message", "test"),
                ("send_file", "/private/document.pdf"),
                ("send_file", "/private/photo.jpg"),
                ("send_audio_message", "/private/audio.mp3"),
                ("send_audio_message", "/private/audio.ogg"),
            ):
                with self.subTest(helper=name, argument=second_argument):
                    self.assertEqual(getattr(whatsapp, name)("123@g.us", second_argument), (False, READ_ONLY_MESSAGE))
            post.assert_not_called()
            isfile.assert_not_called()
            convert.assert_not_called()

    def test_direct_mcp_functions_cannot_bypass_policy(self):
        with patch.object(whatsapp, "READ_ONLY", True), patch.object(whatsapp.requests, "post") as post:
            for name in WRITE_TOOLS:
                with self.subTest(tool=name):
                    result = getattr(main, name)("123", "test")
                    self.assertFalse(result["success"])
                    self.assertEqual(result["message"], READ_ONLY_MESSAGE)
            post.assert_not_called()

    def test_writable_helpers_preserve_payloads(self):
        response = Mock(status_code=200)
        response.json.return_value = {"success": True, "message": "sent"}
        with (
            patch.object(whatsapp, "READ_ONLY", False),
            patch.object(whatsapp.requests, "post", return_value=response) as post,
            patch.object(whatsapp.os.path, "isfile", return_value=True),
            patch.object(whatsapp.audio, "convert_to_opus_ogg_temp", return_value="/tmp/converted.ogg") as convert,
        ):
            for name, argument, expected_payload in (
                ("send_message", "test", {"recipient": "123", "message": "test"}),
                ("send_file", "/tmp/document.pdf", {"recipient": "123", "media_path": "/tmp/document.pdf"}),
                ("send_audio_message", "/tmp/audio.mp3", {"recipient": "123", "media_path": "/tmp/converted.ogg"}),
            ):
                with self.subTest(helper=name):
                    self.assertEqual(getattr(whatsapp, name)("123", argument), (True, "sent"))
                    self.assertEqual(post.call_args.kwargs, {"json": expected_payload})
            convert.assert_called_once_with("/tmp/audio.mp3")

    def test_read_only_still_allows_media_download(self):
        response = Mock(status_code=200)
        response.json.return_value = {"success": True, "path": "/tmp/photo.jpg"}
        with patch.object(whatsapp, "READ_ONLY", True), patch.object(whatsapp.requests, "post", return_value=response) as post:
            self.assertEqual(whatsapp.download_media("media-id", "123@g.us"), "/tmp/photo.jpg")
        post.assert_called_once_with(
            f"{whatsapp.WHATSAPP_API_BASE_URL}/download",
            json={"message_id": "media-id", "chat_jid": "123@g.us"},
        )


if __name__ == "__main__":
    unittest.main()
