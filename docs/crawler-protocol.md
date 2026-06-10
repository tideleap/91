# Crawler Script Protocol v1

Crawler scripts are external processes. The Go backend is the host: it handles
dedupe, downloading, catalog writes, thumbnails, preview videos, fingerprints,
task status and cancellation.

## Invocation

Every script must declare a static crawler name near the top of the Python file.
The admin page reads this value when importing the script; users do not type the
crawler name manually.

```python
CRAWLER_NAME = "Example Crawler"
```

The backend runs:

```bash
python3 /path/to/crawler.py --job /path/to/job.json
```

`job.json`:

```json
{
  "protocol": "crawler.v1",
  "mode": "crawl",
  "run_id": "20260609T120000Z",
  "crawler_id": "example",
  "target_new": 10,
  "seen_source_ids_file": "/data/scriptcrawlers/example/.crawl/seen.txt",
  "output_dir": "/data/scriptcrawlers/example/output",
  "config": {
    "category": "hot"
  },
  "network": {
    "proxy_url": "http://127.0.0.1:7890"
  }
}
```

## Importing Scripts

Crawler scripts are configured from the admin crawler page. A script can be
uploaded as a local file or imported from an HTTP(S) URL.

Imported scripts are copied into `crawler-scripts/` next to the configured local
preview data directory. The import API currently accepts Python files only
(`.py`) and rejects empty files, files larger than 2 MiB, or scripts without
`CRAWLER_NAME`.

## Output

stdout must be JSON Lines. Logs must go to stderr.

Recommended item event:

```json
{
  "type": "item",
  "title": "Video title",
  "media_url": "https://cdn.example.test/video.mp4",
  "thumbnail_url": "https://cdn.example.test/cover.jpg",
  "source_id": "site-native-id",
  "headers": {
    "Referer": "https://example.test/"
  }
}
```

Minimum item event:

```json
{"type":"item","title":"Video title","media_url":"https://cdn.example.test/video.mp4"}
```

If a line contains item fields such as `title` and `media_url`, the backend also
treats it as an item when `type` is omitted.

The item fields may also be wrapped inside `"item"` if that is more convenient:

```json
{"type":"item","item":{"title":"Video title","media_url":"https://cdn.example.test/video.mp4"}}
```

Optional progress/done events:

```json
{"type":"progress","checked":20,"emitted":3}
{"type":"done","stats":{"emitted":10}}
```

## Simple Field Rules

- `title` is required.
- `media_url` is required for normal scripts. The backend downloads the video.
- `thumbnail_url` is optional. If it is empty, the backend generates a thumbnail
  from the downloaded video.
- `source_id` is optional but recommended. If present, it should be stable
  within one crawler and lets the backend skip known videos before downloading.
  If it is empty, the backend creates an internal `auto-...` ID and later relies
  on the existing video fingerprint dedupe path.
- `headers` is optional and is applied to both video and thumbnail downloads.
  Use it for `Referer`, cookies or anti-hotlinking requirements.

## Advanced Fields

- `detail_url`, `author`, `tags`, `category`, `quality`, `duration_seconds`,
  `description` and `published_at` are optional metadata fields.
- If video and thumbnail need different headers, use `media_headers` and
  `thumbnail_headers`.
- Existing nested fields are still supported for compatibility:
  `media.url`, `media.local_file`, `media.headers`, `thumbnail.url`,
  `thumbnail.local_file`, `thumbnail.headers`.
- Advanced scripts may download into `job.output_dir` and return
  `media_local_file` or `media.local_file`. The path must stay inside
  `output_dir`.
- Scripts can read `seen_source_ids_file` and skip known IDs when they provide
  stable `source_id` values. The backend still dedupes every item.
- The backend stops the process after `target_new` new videos are imported.
