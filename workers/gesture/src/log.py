"""Readable console logs and structured JSON files with daily rotation."""
import json
import logging
from datetime import datetime, timezone
from logging.handlers import TimedRotatingFileHandler


class JsonFormatter(logging.Formatter):
    def format(self, record):
        data = dict(timestamp=datetime.fromtimestamp(record.created, timezone.utc).isoformat(),
                    level=record.levelname, logger=record.name,
                    source=f"{record.filename}:{record.lineno}", message=record.getMessage())
        if record.exc_info:
            data["exception"] = self.formatException(record.exc_info)
        return json.dumps(data, ensure_ascii=False)


def setup_logging(config):
    if type(config.backup_days) is not int or config.backup_days < 0:
        raise ValueError("logging.backup_days must be a nonnegative integer")
    if not isinstance(config.level, str) or config.level.upper() not in ("DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL"):
        raise ValueError("logging.level must be DEBUG/INFO/WARNING/ERROR/CRITICAL")
    formatter = JsonFormatter()
    console = logging.StreamHandler()
    console.setFormatter(logging.Formatter("%(asctime)s %(levelname)-7s %(message)s", datefmt="%H:%M:%S"))
    handlers = [console]
    if config.file is not None:
        config.file.parent.mkdir(parents=True, exist_ok=True)
        file_handler = TimedRotatingFileHandler(config.file, when="midnight", interval=1,
                        backupCount=config.backup_days, encoding="utf-8", utc=True)
        file_handler.setFormatter(formatter)
        handlers.append(file_handler)
    logging.basicConfig(level=config.level.upper(), handlers=handlers, force=True)
