import { useState } from "react";
import type { TagItem, VideoDetail } from "@/types";

type Props = {
  video: VideoDetail;
  availableTags?: TagItem[];
  tagSaving?: boolean;
  onTagsChange?: (tags: string[]) => Promise<void>;
};

export function VideoInfoPanel({
  video,
  availableTags = [],
  tagSaving = false,
  onTagsChange,
}: Props) {
  const [collapsed, setCollapsed] = useState(true);
  const [editingTags, setEditingTags] = useState(false);
  const [draftTags, setDraftTags] = useState<string[]>(video.tags ?? []);
  const [tagError, setTagError] = useState("");

  function openTagEditor() {
    setDraftTags(video.tags ?? []);
    setTagError("");
    setEditingTags(true);
  }

  async function saveTags() {
    if (!onTagsChange) return;
    setTagError("");
    try {
      await onTagsChange(draftTags);
      setEditingTags(false);
    } catch (e) {
      setTagError(e instanceof Error ? e.message : "保存标签失败");
    }
  }

  return (
    <section className="info-panel" aria-label="视频信息">
      <header className="info-panel__header">视频信息</header>
      <div className="info-panel__body">
        <div className="info-row">
          <span className="info-row__label">发布时间</span>
          <span className="info-row__value">{video.publishedAt}</span>
        </div>

        {video.sourceLabel && (
          <div className="info-row">
            <span className="info-row__label">来源网盘</span>
            <span className="info-row__value">{video.sourceLabel}</span>
          </div>
        )}

        <div className="info-row">
          <span className="info-row__label">来源/合集</span>
          <div className="info-row__value">
            {video.category || video.author || "未设置"}
          </div>
        </div>

        <div className="info-row">
          <span className="info-row__label">标签</span>
          <div className="info-row__value">
            <div className="detail-tags">
              {(video.tags ?? []).map((t) => (
                <span key={t} className="tag-chip">
                  {t}
                </span>
              ))}
              {onTagsChange && (
                <button className="detail-tags__edit" onClick={openTagEditor}>
                  选择标签
                </button>
              )}
            </div>
            {editingTags && (
              <div className="detail-tag-editor">
                <div className="detail-tag-editor__grid">
                  {availableTags.map((tag) => (
                    <label key={tag.id} className="detail-tag-editor__item">
                      <input
                        type="checkbox"
                        checked={draftTags.includes(tag.label)}
                        onChange={() => setDraftTags(toggleTag(draftTags, tag.label))}
                      />
                      <span>{tag.label}</span>
                      {typeof tag.count === "number" && <em>{tag.count}</em>}
                    </label>
                  ))}
                </div>
                {tagError && <div className="detail-tag-editor__error">{tagError}</div>}
                <div className="detail-tag-editor__actions">
                  <button onClick={() => setEditingTags(false)}>取消</button>
                  <button onClick={saveTags} disabled={tagSaving}>
                    {tagSaving ? "保存中..." : "保存"}
                  </button>
                </div>
              </div>
            )}
          </div>
        </div>

        <div className="info-row">
          <span className="info-row__label">描述</span>
          <span className="info-row__value">
            <p
              className={`description ${collapsed ? "is-collapsed" : ""}`}
            >
              {video.description}
            </p>
            <button
              className="description-toggle"
              onClick={() => setCollapsed((v) => !v)}
            >
              {collapsed ? "展开全文" : "收起"}
            </button>
          </span>
        </div>

      </div>
    </section>
  );
}

function toggleTag(tags: string[], label: string): string[] {
  return tags.includes(label)
    ? tags.filter((tag) => tag !== label)
    : [...tags, label];
}
