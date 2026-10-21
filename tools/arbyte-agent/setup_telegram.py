#!/usr/bin/env python3
"""Safely attach one private Telegram account to the existing NullClaw service.

Run as root on the VPS. The bot token is read with echo disabled and never
written to logs or command arguments. Ownership is proven with a one-time
challenge before the Telegram user ID is allowlisted.
"""
import getpass
import json
import os
from pathlib import Path
import pwd
import secrets
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

HOME = Path("/var/lib/arbyte-agent")
CONFIG = HOME / ".nullclaw/config.json"
ACCOUNT = pwd.getpwnam("arbyte-agent")
UNIT = "arbyte-agent.service"


def telegram(token, method, payload=None):
    url = "https://api.telegram.org/bot" + token + "/" + method
    data = None if payload is None else json.dumps(payload).encode()
    req = urllib.request.Request(url, data=data,
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=35) as response:
            obj = json.load(response)
    except Exception:
        # Avoid printing urllib exceptions: they may contain the token URL.
        raise RuntimeError("Telegram API tidak bisa dihubungi; detail disembunyikan demi keamanan token") from None
    if not obj.get("ok"):
        raise RuntimeError("Telegram menolak permintaan; token atau konfigurasi bot perlu diperiksa")
    return obj["result"]


def validate_config():
    env = os.environ.copy()
    env.update({"HOME": str(HOME), "NULLCLAW_HOME": str(HOME / ".nullclaw"),
                "NULLCLAW_WORKSPACE": str(HOME / ".nullclaw/workspace")})
    result = subprocess.run(["runuser", "-u", "arbyte-agent", "--", "env",
                             "HOME=" + str(HOME),
                             "NULLCLAW_HOME=" + str(HOME / ".nullclaw"),
                             "NULLCLAW_WORKSPACE=" + str(HOME / ".nullclaw/workspace"),
                             "/usr/local/bin/nullclaw", "config", "validate"],
                            env=env, stdout=subprocess.DEVNULL,
                            stderr=subprocess.DEVNULL, check=False)
    if result.returncode:
        raise RuntimeError("Validasi konfigurasi NullClaw gagal")


def main():
    if os.geteuid() != 0:
        raise RuntimeError("Jalankan melalui sudo pada VPS")
    if not CONFIG.is_file():
        raise RuntimeError("Konfigurasi NullClaw tidak ditemukan")

    token = getpass.getpass("Token bot BARU dari BotFather (input tersembunyi): ").strip()
    if ":" not in token or len(token) < 30:
        raise RuntimeError("Format token tidak valid")
    bot = telegram(token, "getMe")
    if not bot.get("is_bot") or not bot.get("username"):
        raise RuntimeError("Akun Telegram ini bukan bot yang valid")

    challenge = secrets.token_urlsafe(12)
    print("\nBot terverifikasi: @" + bot["username"])
    print("Buka bot itu dan kirim pesan ini dalam chat pribadi:")
    print("/start " + challenge)
    print("Menunggu hingga 3 menit; hanya pesan dengan challenge ini yang diterima.")

    deadline = time.monotonic() + 180
    offset = None
    user_id = None
    while time.monotonic() < deadline:
        args = {"timeout": 25, "allowed_updates": ["message"]}
        if offset is not None:
            args["offset"] = offset
        for update in telegram(token, "getUpdates", args):
            offset = max(offset or 0, update["update_id"] + 1)
            message = update.get("message", {})
            text = (message.get("text") or "").strip().split()
            chat = message.get("chat", {})
            sender = message.get("from", {})
            if (chat.get("type") == "private" and text == ["/start", challenge]
                    and sender.get("id") == chat.get("id")):
                user_id = str(sender["id"])
                break
        if user_id:
            break
    if not user_id:
        raise RuntimeError("Challenge belum diterima; konfigurasi tidak diubah")

    config = json.loads(CONFIG.read_text())
    channels = config.setdefault("channels", {})
    telegram_cfg = channels.setdefault("telegram", {})
    accounts = telegram_cfg.setdefault("accounts", {})
    if "main" in accounts:
        raise RuntimeError("Akun Telegram 'main' sudah ada; konfigurasi tidak diubah")
    accounts["main"] = {
        "bot_token": token,
        "allow_from": [user_id],
        "group_allow_from": [],
        "streaming": True,
        "require_mention": True,
    }

    backup = CONFIG.with_name("config.json.pre-telegram-" + time.strftime("%Y%m%dT%H%M%SZ", time.gmtime()))
    shutil.copy2(CONFIG, backup)
    os.chown(backup, ACCOUNT.pw_uid, ACCOUNT.pw_gid)
    os.chmod(backup, 0o600)
    temp = CONFIG.with_suffix(".json.tmp")
    temp.write_text(json.dumps(config, indent=2) + "\n")
    os.chown(temp, ACCOUNT.pw_uid, ACCOUNT.pw_gid)
    os.chmod(temp, 0o600)
    os.replace(temp, CONFIG)
    try:
        validate_config()
        subprocess.run(["systemctl", "restart", UNIT], check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        active = subprocess.run(["systemctl", "is-active", "--quiet", UNIT], check=False)
        if active.returncode:
            raise RuntimeError("Layanan NullClaw tidak aktif setelah restart")
    except Exception:
        shutil.copy2(backup, CONFIG)
        os.chown(CONFIG, ACCOUNT.pw_uid, ACCOUNT.pw_gid)
        os.chmod(CONFIG, 0o600)
        subprocess.run(["systemctl", "restart", UNIT], check=False,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        raise

    print("Telegram terhubung ke NullClaw; akses hanya untuk user ID " + user_id + ".")
    print("Layanan aktif. Backup konfigurasi: " + str(backup))
    print("Kirim /start lagi ke bot, lalu coba 'status NodeBank'.")


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        print("Dibatalkan; konfigurasi tidak diubah.", file=sys.stderr)
        sys.exit(130)
    except Exception as error:
        print("Gagal menyiapkan Telegram: " + str(error), file=sys.stderr)
        sys.exit(1)
