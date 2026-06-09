-- ABOUTME: Recreate source_files with composite primary key (path, source)
-- ABOUTME: Prevents collisions when different sources scan overlapping file paths

CREATE TABLE source_files_new (
    path TEXT NOT NULL,
    source TEXT NOT NULL DEFAULT 'claude-code',
    mtime DATETIME NOT NULL,
    synced_at DATETIME NOT NULL,
    PRIMARY KEY (path, source)
);

INSERT INTO source_files_new (path, source, mtime, synced_at)
    SELECT path,
           COALESCE(NULLIF(source, ''), 'claude-code') AS source,
           mtime,
           synced_at
    FROM source_files;

DROP TABLE source_files;

ALTER TABLE source_files_new RENAME TO source_files;
