import os
from pathlib import Path
import subprocess
import sys
import unittest
from unittest.mock import Mock, patch

import whatsapp


class BridgeConfigurationTests(unittest.TestCase):
    def test_custom_url_is_loaded_at_startup(self):
        result = subprocess.run(
            [sys.executable, "-c", "import whatsapp; print(whatsapp.WHATSAPP_API_BASE_URL)"],
            cwd=Path(__file__).resolve().parent,
            env={**os.environ, "WHATSAPP_READ_ONLY": "true", "WHATSAPP_API_BASE_URL": "http://127.0.0.1:8081/api/"},
            capture_output=True, text=True, timeout=15,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), "http://127.0.0.1:8081/api")

    def test_base_url_validation(self):
        self.assertEqual(whatsapp.parse_bridge_api_base_url(None), "http://127.0.0.1:8080/api")
        self.assertEqual(whatsapp.parse_bridge_api_base_url(" https://localhost:8443/whatsapp/api/ "), "https://localhost:8443/whatsapp/api")
        for value in ("", "localhost:8081/api", "ftp://localhost/api", "http://localhost:0/api", "http://localhost:65536/api", "http://localhost:bad/api", "http://localhost/api?x=1", "http://localhost/api#fragment"):
            with self.subTest(value=value):
                with self.assertRaisesRegex(ValueError, "WHATSAPP_API_BASE_URL"):
                    whatsapp.parse_bridge_api_base_url(value)

    def test_http_helpers_use_the_selected_bridge(self):
        response = Mock(status_code=200)
        response.json.return_value = {"success": True, "message": "sent", "path": "/tmp/media.jpg"}
        with (
            patch.object(whatsapp, "READ_ONLY", False),
            patch.object(whatsapp, "WHATSAPP_API_BASE_URL", "http://127.0.0.1:8081/api"),
            patch.object(whatsapp.requests, "post", return_value=response) as post,
        ):
            self.assertEqual(whatsapp.send_message("123", "test"), (True, "sent"))
            post.assert_called_with("http://127.0.0.1:8081/api/send", json={"recipient": "123", "message": "test"})
            self.assertEqual(whatsapp.download_media("media-id", "123@g.us"), "/tmp/media.jpg")
            post.assert_called_with("http://127.0.0.1:8081/api/download", json={"message_id": "media-id", "chat_jid": "123@g.us"})
