import argparse
import json
import os
from pathlib import Path


def check(home, model):
    if not __debug__:
        raise RuntimeError("native configuration checks require Python assertions enabled")
    from hermes_constants import get_default_hermes_root, get_hermes_home
    from hermes_cli.auth import _codex_access_token_is_expiring, _load_auth_store
    from hermes_cli.config import DEFAULT_CONFIG, load_config
    from hermes_cli.runtime_provider import resolve_runtime_provider

    home = Path(home)
    assert home.is_absolute() and not home.is_symlink(), "Hermes home must be absolute and not a symlink"
    assert home.stat().st_uid == os.getuid() and home.stat().st_mode & 0o077 == 0, "Hermes home must be owner-only"
    assert get_hermes_home().resolve() == home.resolve(), "unexpected native Hermes home"
    assert get_default_hermes_root().resolve() == home.resolve(), "global profile fallback must be isolated"
    auth_path = home / "auth.json"
    assert auth_path.is_file() and not auth_path.is_symlink(), "auth store must be a regular file"
    assert auth_path.stat().st_uid == os.getuid() and auth_path.stat().st_mode & 0o077 == 0, "auth store must be owner-only"
    store = _load_auth_store()
    assert not store.get("providers"), "no singleton provider credentials are permitted in this acceptance environment"
    assert set(store.get("credential_pool", {})) == {"openai-codex"}, "only the explicit OpenAI pool is permitted"
    entries = store["credential_pool"]["openai-codex"]
    assert len(entries) == 1, "acceptance requires exactly one access-only credential"
    credential = entries[0]
    assert credential.get("auth_type") == "api_key", "OAuth pool entries can refresh shared credentials"
    assert credential.get("source") == "manual", "credential must be explicitly imported"
    assert not credential.get("refresh_token") and not credential.get("id_token"), "refresh and ID tokens must not be copied"
    access_token = credential.get("access_token")
    assert isinstance(access_token, str) and access_token, "access token missing"
    assert not _codex_access_token_is_expiring(access_token, 1800), "access token expires within the acceptance window"
    config = load_config()
    assert config["model"]["provider"] == "openai-codex", "main provider is not explicit OpenAI"
    assert config["model"]["default"] == model, "configured model differs from requested model"
    assert not config.get("fallback_providers") and not config.get("fallback_model"), "provider fallbacks must be empty"
    assert set(DEFAULT_CONFIG["auxiliary"]).issubset(config["auxiliary"]), "auxiliary routing is incomplete"
    for task in config["auxiliary"].values():
        assert task.get("provider") == "openai-codex" and task.get("model") == model, "auxiliary traffic must use explicit OpenAI"
        assert not task.get("fallback_chain") and not task.get("base_url") and not task.get("api_key"), "auxiliary overrides must not change routing"
    runtime = resolve_runtime_provider(requested="openai-codex")
    assert runtime["provider"] == "openai-codex" and runtime["api_mode"] == "codex_responses", "unexpected native transport"
    assert runtime["base_url"] == "https://chatgpt.com/backend-api/codex", "unexpected native endpoint"
    assert runtime["api_key"] == access_token, "native runtime selected another credential"
    print(json.dumps({"provider": runtime["provider"], "model": model, "api_mode": runtime["api_mode"], "access_only": True, "refresh_token_absent": True, "private_store": True, "global_profile_fallback_absent": True, "auxiliary_openai_only": True}))


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--home", required=True)
    parser.add_argument("--model", required=True)
    arguments = parser.parse_args()
    check(arguments.home, arguments.model)
