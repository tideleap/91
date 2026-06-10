#!/usr/bin/env python3
import argparse
import json
import sys

CRAWLER_NAME = "Demo Crawler"


def load_seen(path):
    try:
        with open(path, "r", encoding="utf-8") as f:
            return {line.strip() for line in f if line.strip()}
    except FileNotFoundError:
        return set()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--job", required=True)
    args = parser.parse_args()

    with open(args.job, "r", encoding="utf-8") as f:
        job = json.load(f)

    seen = load_seen(job.get("seen_source_ids_file", ""))
    source_id = "demo-video-1"
    if source_id in seen:
        print(json.dumps({"type": "done", "stats": {"emitted": 0}}), flush=True)
        return

    event = {
        "type": "item",
        "source_id": source_id,
        "title": "Demo Video",
        "media_url": "https://example.test/video/demo-video-1.mp4",
        "thumbnail_url": "https://example.test/thumb/demo-video-1.jpg",
        "headers": {
            "Referer": "https://example.test/",
        },
    }
    print(json.dumps(event, ensure_ascii=False), flush=True)
    print(json.dumps({"type": "done", "stats": {"emitted": 1}}), flush=True)


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        print(f"crawler failed: {exc}", file=sys.stderr, flush=True)
        raise
