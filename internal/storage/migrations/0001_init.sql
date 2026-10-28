CREATE TABLE IF NOT EXISTS targets (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	name             TEXT    NOT NULL,
	url              TEXT    NOT NULL,
	interval_seconds INTEGER NOT NULL,
	timeout_seconds  INTEGER NOT NULL,
	enabled          INTEGER NOT NULL DEFAULT 1,
	created_at       INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS check_results (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	target_id   INTEGER NOT NULL REFERENCES targets (id) ON DELETE CASCADE,
	status      TEXT    NOT NULL,
	status_code INTEGER NOT NULL DEFAULT 0,
	latency_ms  INTEGER NOT NULL,
	error       TEXT    NOT NULL DEFAULT '',
	checked_at  INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_results_target_id ON check_results (target_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_results_checked_at ON check_results (checked_at);
