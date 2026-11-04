"""Structured JSON log formatter and setup."""
import json
import logging
import logging.handlers
from datetime import datetime, timezone

from mediapipe_video.config import LogConfig


class _JsonFormatter(logging.Formatter):
    def format(self, record: logging.LogRecord) -> str:
        return json.dumps({
            "timestamp": datetime.fromtimestamp(record.created, tz=timezone.utc).isoformat(),
            "level": record.levelname,
            "logger": record.name,
            "source": f"{record.filename}:{record.lineno}",
            "message": record.getMessage(),
        })


def setup_logging(cfg: LogConfig) -> None:
    formatter = _JsonFormatter()

    handlers: list[logging.Handler] = [logging.StreamHandler()]

    if cfg.file is not None:
        cfg.file.parent.mkdir(parents=True, exist_ok=True)
        handlers.append(logging.handlers.TimedRotatingFileHandler(
            cfg.file,
            when="midnight",
            interval=1,
            backupCount=cfg.backup_days,
            encoding="utf-8",
            utc=True,
        ))

    for h in handlers:
        h.setFormatter(formatter)

    logging.root.setLevel(cfg.level.upper())
    logging.root.handlers = handlers
