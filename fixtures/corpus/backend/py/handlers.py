"""Decorated functions, including a multi-line signature.

Every decorated function was indexed twice — once at the decorator, once at the
`def` — which was 4,224 duplicate rows on a real Python service. Undecorated
functions were never affected, which is why fixtures missed it.
"""
import os

import pytest

TIMEOUT_SECONDS = 30


@pytest.mark.asyncio
async def test_retry_audit_log_written(
    db, storage, monkeypatch
):
    assert db is not None


@pytest.mark.asyncio
def decorated_one_line(x): return x


def undecorated(a, b):
    return os.environ.get("PY_ONLY_KEY", a + b)
