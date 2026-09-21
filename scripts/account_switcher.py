#!/usr/bin/env python3
"""
Antigravity (AGY) & Codex / Copilot Account Switcher CLI
Allows AI agents and users to list, switch, add, and backup multiple AGY accounts with refresh tokens.
"""

from __future__ import annotations

import argparse
import json
import os
import pathlib
import shutil
import sys
from typing import Any, Dict

# Force UTF-8 stdout if possible
if hasattr(sys.stdout, "reconfigure"):
    try:
        sys.stdout.reconfigure(encoding="utf-8")
    except Exception:
        pass

GEMINI_DIR = pathlib.Path.home() / ".gemini"
ACCOUNTS_STORE_DIR = GEMINI_DIR / "accounts"
ACCOUNTS_INDEX_FILE = GEMINI_DIR / "google_accounts.json"
OAUTH_CREDS_FILE = GEMINI_DIR / "oauth_creds.json"
CODEX_SWITCHER_SCRIPT = pathlib.Path(r"D:\Andrew\Code\Github\codex-switcher\codex_switcher.py")


def ensure_dirs():
    ACCOUNTS_STORE_DIR.mkdir(parents=True, exist_ok=True)


def get_current_active_email() -> str:
    if ACCOUNTS_INDEX_FILE.exists():
        try:
            data = json.loads(ACCOUNTS_INDEX_FILE.read_text(encoding="utf-8"))
            return data.get("active", "")
        except Exception:
            pass
    return ""


def save_current_as_profile(name: str | None = None) -> bool:
    ensure_dirs()
    if not OAUTH_CREDS_FILE.exists():
        print(f"Error: {OAUTH_CREDS_FILE} does not exist.")
        return False

    current_email = get_current_active_email()
    target_name = name or current_email or "default"
    
    target_file = ACCOUNTS_STORE_DIR / f"{target_name}.json"
    shutil.copyfile(OAUTH_CREDS_FILE, target_file)
    print(f"[OK] Saved current active credentials as profile: '{target_name}' -> {target_file}")
    return True


def list_profiles():
    ensure_dirs()
    # If store is empty but current oauth_creds exists, auto-save current
    profiles = list(ACCOUNTS_STORE_DIR.glob("*.json"))
    if not profiles and OAUTH_CREDS_FILE.exists():
        save_current_as_profile()
        profiles = list(ACCOUNTS_STORE_DIR.glob("*.json"))

    active_email = get_current_active_email()
    print("==================================================")
    print("   Antigravity (AGY) Registered Account Profiles  ")
    print("==================================================")
    
    if not profiles:
        print("  (No account profiles stored yet in ~/.gemini/accounts/)")
        print("  Run: agy-switch save <name> to save current login.")
        return

    for p in sorted(profiles, key=lambda x: x.name):
        name = p.stem
        is_active = (name == active_email)
        status_tag = " [ACTIVE]" if is_active else ""
        
        # Read token preview
        token_preview = "N/A"
        try:
            d = json.loads(p.read_text(encoding="utf-8"))
            rt = d.get("refresh_token", "")
            if rt:
                token_preview = f"{rt[:8]}...{rt[-4:]}"
        except Exception:
            pass

        print(f"  * {name:<30} (Token: {token_preview}){status_tag}")
    
    print("==================================================")
    print(f"Current Active Account: {active_email or 'Unknown'}")


def switch_to_profile(name: str) -> bool:
    ensure_dirs()
    target_file = ACCOUNTS_STORE_DIR / f"{name}.json"
    
    # Try exact match or match without .json
    if not target_file.exists():
        candidates = list(ACCOUNTS_STORE_DIR.glob(f"*{name}*.json"))
        if len(candidates) == 1:
            target_file = candidates[0]
            name = target_file.stem
        else:
            print(f"Error: Account profile '{name}' not found in {ACCOUNTS_STORE_DIR}")
            list_profiles()
            return False

    # Auto backup current active before switching
    cur_email = get_current_active_email()
    if cur_email and OAUTH_CREDS_FILE.exists():
        cur_file = ACCOUNTS_STORE_DIR / f"{cur_email}.json"
        if not cur_file.exists():
            shutil.copyfile(OAUTH_CREDS_FILE, cur_file)

    # 1. Update oauth_creds.json
    shutil.copyfile(target_file, OAUTH_CREDS_FILE)

    # 2. Update google_accounts.json
    try:
        index_data = {"active": name, "old": []}
        if ACCOUNTS_INDEX_FILE.exists():
            try:
                old_data = json.loads(ACCOUNTS_INDEX_FILE.read_text(encoding="utf-8"))
                old_list = old_data.get("old", [])
                if cur_email and cur_email not in old_list and cur_email != name:
                    old_list.append(cur_email)
                index_data["old"] = old_list
            except Exception:
                pass
        ACCOUNTS_INDEX_FILE.write_text(json.dumps(index_data, indent=2), encoding="utf-8")
    except Exception as e:
        print(f"Warning: Failed to update google_accounts.json: {e}")

    print(f"[OK] Successfully switched Antigravity active account to: {name}")
    return True


def add_account(name: str, refresh_token: str, access_token: str = "", id_token: str = "") -> bool:
    ensure_dirs()
    creds_template = {
        "access_token": access_token or "dummy_access_token",
        "refresh_token": refresh_token,
        "token_type": "Bearer",
        "expiry_date": 0,
        "id_token": id_token,
        "scope": "https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/userinfo.profile openid https://www.googleapis.com/auth/cloud-platform"
    }

    # If current oauth_creds exists, preserve client scope & structure
    if OAUTH_CREDS_FILE.exists():
        try:
            base = json.loads(OAUTH_CREDS_FILE.read_text(encoding="utf-8"))
            base["refresh_token"] = refresh_token
            if access_token:
                base["access_token"] = access_token
            if id_token:
                base["id_token"] = id_token
            creds_template = base
        except Exception:
            pass

    target_file = ACCOUNTS_STORE_DIR / f"{name}.json"
    target_file.write_text(json.dumps(creds_template, indent=2), encoding="utf-8")
    print(f"[OK] Successfully added account profile '{name}' (Refresh Token registered).")
    return True


def delete_profile(name: str) -> bool:
    ensure_dirs()
    target_file = ACCOUNTS_STORE_DIR / f"{name}.json"
    if target_file.exists():
        target_file.unlink()
        print(f"[OK] Removed account profile: '{name}'")
        return True
    else:
        print(f"Error: Profile '{name}' not found.")
        return False


def main():
    parser = argparse.ArgumentParser(description="Antigravity (AGY) & Multi-Account Switcher")
    subparsers = parser.add_subparsers(dest="action", help="Action to perform")

    # list / status
    subparsers.add_parser("list", help="List all available account profiles")
    subparsers.add_parser("status", help="Show current active account")

    # use / switch
    use_p = subparsers.add_parser("use", help="Switch active account to profile name/email")
    use_p.add_argument("name", help="Profile name or email")

    # save
    save_p = subparsers.add_parser("save", help="Save current login session as a named profile")
    save_p.add_argument("name", nargs="?", default=None, help="Optional profile name (defaults to active email)")

    # add
    add_p = subparsers.add_parser("add", help="Add a new account with a refresh token")
    add_p.add_argument("name", help="Profile name or email")
    add_p.add_argument("--refresh-token", "-r", required=True, help="Google / Antigravity OAuth Refresh Token")
    add_p.add_argument("--access-token", "-a", default="", help="Optional Access Token")
    add_p.add_argument("--id-token", "-i", default="", help="Optional ID Token")

    # delete
    del_p = subparsers.add_parser("delete", help="Delete a stored profile")
    del_p.add_argument("name", help="Profile name or email to remove")

    # codex
    codex_p = subparsers.add_parser("codex", help="Switch Codex provider (ekti, 9router, etc.)")
    codex_p.add_argument("provider", nargs="?", default=None, help="Provider name (e.g. ekti, 9router)")

    args = parser.parse_args()

    if not args.action or args.action in ("list", "status"):
        list_profiles()
    elif args.action == "use":
        switch_to_profile(args.name)
    elif args.action == "save":
        save_current_as_profile(args.name)
    elif args.action == "add":
        add_account(args.name, args.refresh_token, args.access_token, args.id_token)
    elif args.action == "delete":
        delete_profile(args.name)
    elif args.action == "codex":
        if CODEX_SWITCHER_SCRIPT.exists():
            import subprocess
            cmd = [sys.executable, str(CODEX_SWITCHER_SCRIPT)]
            if args.provider:
                cmd.extend(["--provider", args.provider])
            subprocess.run(cmd)
        else:
            print(f"Codex switcher script not found at {CODEX_SWITCHER_SCRIPT}")


if __name__ == "__main__":
    main()
