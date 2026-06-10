import { useEffect, useMemo, useState, type ReactNode } from "react";
import {
  Activity,
  ArrowLeft,
  ChevronRight,
  CircleStop,
  Clock,
  Download,
  FileCode2,
  Gauge,
  Link as LinkIcon,
  Plus,
  RefreshCw,
  Save,
  Settings2,
  TestTube,
  Trash2,
  Upload,
} from "lucide-react";
import * as api from "./api";
import { useToast } from "./ToastContext";
import { generationStateClass, generationStateLabel } from "./drive/constants";
import { SpiderIcon } from "./icons/SpiderIcon";

type CrawlerForm = {
  id: string;
  name: string;
  scriptPath: string;
  targetNew: string;
  proxy: string;
};

const emptyForm: CrawlerForm = {
  id: "",
  name: "",
  scriptPath: "",
  targetNew: "10",
  proxy: "",
};

export function CrawlersPage() {
  const [list, setList] = useState<api.AdminCrawler[]>([]);
  const [selectedId, setSelectedId] = useState("");
  const [form, setForm] = useState<CrawlerForm>(emptyForm);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [runningId, setRunningId] = useState("");
  const [stoppingId, setStoppingId] = useState("");
  const [scriptURL, setScriptURL] = useState("");
  const [importingScript, setImportingScript] = useState(false);
  const [testingScript, setTestingScript] = useState(false);
  const [testResult, setTestResult] = useState<api.CrawlerDryRunResult | null>(null);
  const [mode, setMode] = useState<"list" | "detail">("list");
  const { show } = useToast();

  const selected = useMemo(
    () => list.find((item) => item.id === selectedId) ?? null,
    [list, selectedId]
  );
  const stats = useMemo(() => {
    const running = list.filter((item) => item.scanGenerationStatus?.state === "scanning").length;
    return {
      total: list.length,
      ready: list.filter((item) => item.status === "ok").length,
      running,
      error: list.filter((item) => item.status === "error").length,
    };
  }, [list]);

  async function refresh() {
    setLoading(true);
    try {
      const data = await api.listCrawlers();
      setList(data);
    } catch (e) {
      show(e instanceof Error ? e.message : "加载爬虫失败", "error");
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => {
    refresh();
  }, []);

  function selectCrawler(crawler: api.AdminCrawler) {
    setSelectedId(crawler.id);
    setMode("detail");
    setTestResult(null);
    setForm({
      id: crawler.id,
      name: crawler.name,
      scriptPath: crawler.scriptPath ?? "",
      targetNew: crawler.targetNew || "10",
      proxy: crawler.proxy ?? "",
    });
  }

  function createCustom() {
    setSelectedId("");
    setForm(emptyForm);
    setScriptURL("");
    setTestResult(null);
    setMode("detail");
  }

  function backToList() {
    setSelectedId("");
    setForm(emptyForm);
    setScriptURL("");
    setTestResult(null);
    setMode("list");
  }

  function set<K extends keyof CrawlerForm>(key: K, value: CrawlerForm[K]) {
    setForm((prev) => ({ ...prev, [key]: value }));
  }

  async function save() {
    const id = form.id.trim();
    if (!form.scriptPath.trim()) {
      show("请先导入爬虫脚本", "error");
      return;
    }
    setSaving(true);
    try {
      const resp = await api.upsertCrawler({
        id: id || undefined,
        scriptPath: form.scriptPath.trim(),
        targetNew: form.targetNew.trim(),
        proxy: form.proxy.trim(),
      });
      if (resp.warning) {
        show(`已保存，但初始化失败：${resp.warning}`, "error");
      } else {
        show("已保存", "success");
      }
      setSelectedId(resp.id || id);
      await refresh();
      setMode("list");
    } catch (e) {
      show(e instanceof Error ? e.message : "保存失败", "error");
    } finally {
      setSaving(false);
    }
  }

  async function importScriptFile(file: File | null | undefined) {
    if (!file) return;
    setImportingScript(true);
    try {
      const resp = await api.importCrawlerScriptFile(file);
      set("scriptPath", resp.scriptPath);
      set("name", resp.name);
      setTestResult(null);
      show("脚本已导入", "success");
    } catch (e) {
      show(e instanceof Error ? e.message : "导入失败", "error");
    } finally {
      setImportingScript(false);
    }
  }

  async function importScriptURL() {
    const url = scriptURL.trim();
    if (!url) {
      show("请填写脚本链接", "error");
      return;
    }
    setImportingScript(true);
    try {
      const resp = await api.importCrawlerScriptURL(url);
      set("scriptPath", resp.scriptPath);
      set("name", resp.name);
      setScriptURL("");
      setTestResult(null);
      show("脚本已导入", "success");
    } catch (e) {
      show(e instanceof Error ? e.message : "导入失败", "error");
    } finally {
      setImportingScript(false);
    }
  }

  async function testScript() {
    const scriptPath = form.scriptPath.trim();
    if (!scriptPath) {
      show("请先导入爬虫脚本", "error");
      return;
    }
    setTestingScript(true);
    setTestResult(null);
    try {
      const result = await api.testCrawlerScript({
        scriptPath,
        proxy: form.proxy.trim(),
      });
      setTestResult(result);
      if (result.ok) {
        show("测试通过", "success");
      } else {
        show(crawlerTestFailure(result) || "测试失败", "error");
      }
    } catch (e) {
      show(e instanceof Error ? e.message : "测试失败", "error");
    } finally {
      setTestingScript(false);
    }
  }

  async function run(crawler: api.AdminCrawler) {
    setRunningId(crawler.id);
    try {
      const resp = await api.runCrawler(crawler.id);
      if (!resp.accepted) {
        show(resp.message || "当前爬虫有正在进行的任务", "info");
        return;
      }
      show("已触发抓取任务", "success");
      await refresh();
    } catch (e) {
      show(e instanceof Error ? e.message : "触发失败", "error");
    } finally {
      setRunningId("");
    }
  }

  async function stop(crawler: api.AdminCrawler) {
    setStoppingId(crawler.id);
    try {
      const resp = await api.stopCrawlerTasks(crawler.id);
      show(resp.stopped ? "已请求停止任务" : "当前没有可停止任务", "info");
      await refresh();
    } catch (e) {
      show(e instanceof Error ? e.message : "停止失败", "error");
    } finally {
      setStoppingId("");
    }
  }

  async function remove(crawler: api.AdminCrawler) {
    if (!window.confirm(`删除爬虫 ${crawler.name} 的脚本和配置？已爬取的视频会保留。`)) return;
    try {
      const resp = await api.deleteCrawler(crawler.id);
      if (resp.warning) {
        show(`已删除爬虫配置，但脚本文件清理失败：${resp.warning}`, "error");
      } else if (resp.deletedScript) {
        show("已删除爬虫配置和脚本文件，已爬取视频保留", "success");
      } else {
        show("已删除爬虫配置，已爬取视频保留", "success");
      }
      setSelectedId("");
      setForm(emptyForm);
      setMode("list");
      await refresh();
    } catch (e) {
      show(e instanceof Error ? e.message : "删除失败", "error");
    }
  }

  return (
    <section className="admin-page">
      <header className="admin-page__header">
        <div>
          <h1 className="admin-page__title">爬虫管理</h1>
        </div>
        <div className="admin-detail-actions-inline">
          {mode === "list" ? (
            <button className="admin-btn is-primary" onClick={createCustom}>
              <Plus size={14} /> 添加爬虫
            </button>
          ) : (
            <button className="admin-btn" onClick={backToList}>
              <ArrowLeft size={14} /> 返回列表
            </button>
          )}
        </div>
      </header>

      {mode === "list" ? (
        <div className="admin-crawler-console">
          <div className="admin-crawler-overview">
            <CrawlerMetric label="已配置" value={stats.total} icon={<SpiderIcon size={16} />} />
            <CrawlerMetric label="已就绪" value={stats.ready} icon={<Activity size={16} />} tone="ok" />
            <CrawlerMetric label="抓取中" value={stats.running} icon={<RefreshCw size={16} />} tone="info" />
            <CrawlerMetric label="错误" value={stats.error} icon={<CircleStop size={16} />} tone="error" />
          </div>

          <div className="admin-card admin-crawler-list">
            <div className="admin-crawler-list__head">
              <header className="admin-card__title">
                <SpiderIcon size={16} /> 已配置爬虫
              </header>
              <button className="admin-btn" type="button" onClick={refresh} disabled={loading}>
                <RefreshCw size={13} className={loading ? "admin-spin" : undefined} /> 刷新
              </button>
            </div>
            {loading ? (
              <div className="admin-loading-state">
                <RefreshCw size={18} className="admin-spin" />
                <span>加载中...</span>
              </div>
            ) : list.length === 0 ? (
              <div className="admin-crawler-empty">
                <SpiderIcon size={28} />
                <strong>暂无爬虫</strong>
                <button className="admin-btn is-primary" type="button" onClick={createCustom}>
                  <Plus size={13} /> 添加爬虫
                </button>
              </div>
            ) : (
              <div className="admin-crawler-table">
                {list.map((crawler) => (
                  <CrawlerRow
                    key={crawler.id}
                    crawler={crawler}
                    active={crawler.id === selectedId}
                    running={runningId === crawler.id}
                    stopping={stoppingId === crawler.id}
                    onSelect={() => selectCrawler(crawler)}
                    onRun={() => run(crawler)}
                    onStop={() => stop(crawler)}
                  />
                ))}
              </div>
            )}
          </div>
        </div>
      ) : (
        <div className="admin-crawler-editor">
          <div className="admin-crawler-editor__main">
            <div className="admin-crawler-section">
              <div className="admin-crawler-section__head">
                <span className="admin-crawler-section__icon"><Settings2 size={15} /></span>
                <span className="admin-crawler-section__title">基础信息</span>
              </div>
              <div className="admin-crawler-script-name">
                <span>脚本名称</span>
                <strong>{form.name || "导入脚本后自动读取"}</strong>
              </div>
            </div>

            <div className="admin-crawler-section">
              <div className="admin-crawler-section__head">
                <span className="admin-crawler-section__icon"><FileCode2 size={15} /></span>
                <span className="admin-crawler-section__title">脚本导入与测试</span>
              </div>
              <div className="admin-form">
                <div className="admin-form__row">
                  <label htmlFor="crawler-script-url">导入脚本</label>
                  <div className="admin-crawler-import">
                    <input
                      id="crawler-script-file"
                      className="admin-crawler-import__file"
                      type="file"
                      accept=".py,text/x-python"
                      disabled={importingScript}
                      onChange={(e) => {
                        importScriptFile(e.target.files?.[0]);
                        e.currentTarget.value = "";
                      }}
                    />
                    <label className="admin-btn" htmlFor="crawler-script-file" aria-disabled={importingScript}>
                      <Upload size={13} /> 上传文件
                    </label>
                    <input
                      id="crawler-script-url"
                      value={scriptURL}
                      onChange={(e) => setScriptURL(e.target.value)}
                      placeholder="https://example.com/crawler.py"
                      disabled={importingScript}
                    />
                    <button className="admin-btn" type="button" onClick={importScriptURL} disabled={importingScript}>
                      <LinkIcon size={13} /> {importingScript ? "导入中..." : "链接导入"}
                    </button>
                    <button
                      className="admin-btn"
                      type="button"
                      onClick={testScript}
                      disabled={!form.scriptPath || importingScript || testingScript}
                    >
                      <TestTube size={13} /> {testingScript ? "测试中..." : "测试脚本"}
                    </button>
                  </div>
                  {form.scriptPath && <div className="admin-form__help">脚本已导入</div>}
                  {testResult && <CrawlerTestResult result={testResult} />}
                </div>
              </div>
            </div>

            <div className="admin-crawler-section">
              <div className="admin-crawler-section__head">
                <span className="admin-crawler-section__icon"><Gauge size={15} /></span>
                <span className="admin-crawler-section__title">运行参数</span>
              </div>
              <div className="admin-crawler-params">
                <div className="admin-form__row">
                  <label htmlFor="crawler-target">每次补充新视频数</label>
                  <input id="crawler-target" value={form.targetNew} onChange={(e) => set("targetNew", e.target.value)} placeholder="10" />
                </div>
                <div className="admin-form__row">
                  <label htmlFor="crawler-proxy">代理地址</label>
                  <input
                    id="crawler-proxy"
                    value={form.proxy}
                    onChange={(e) => {
                      set("proxy", e.target.value);
                      setTestResult(null);
                    }}
                    placeholder="http://127.0.0.1:7890"
                  />
                </div>
              </div>
            </div>
          </div>

          <aside className="admin-crawler-editor__side">
            <div className="admin-crawler-action-panel">
              <div className="admin-crawler-action-panel__head">
                <span className="admin-crawler-action-panel__mark">
                  <SpiderIcon size={18} />
                </span>
                <div>
                  <strong>{selected ? "爬虫配置" : "添加爬虫"}</strong>
                  <span>{selected ? crawlerStatusLabel(selected) : "未保存"}</span>
                </div>
              </div>
              <div className="admin-crawler-action-panel__buttons">
                <button className="admin-btn is-primary" onClick={save} disabled={saving}>
                  <Save size={13} /> {saving ? "保存中..." : "保存"}
                </button>
                {selected && (
                  <>
                    <button className="admin-btn" onClick={() => run(selected)} disabled={runningId === selected.id}>
                      <Download size={13} /> {runningId === selected.id ? "触发中..." : "立即抓取"}
                    </button>
                    <button className="admin-btn is-stop" onClick={() => stop(selected)} disabled={stoppingId === selected.id}>
                      <CircleStop size={13} /> {stoppingId === selected.id ? "停止中..." : "停止任务"}
                    </button>
                    <button className="admin-btn is-danger" onClick={() => remove(selected)}>
                      <Trash2 size={13} /> 删除
                    </button>
                  </>
                )}
              </div>
            </div>

            {selected && (
              <div className="admin-crawler-side-panel">
                <div className="admin-crawler-section__head">
                  <span className="admin-crawler-section__icon"><Activity size={15} /></span>
                  <span className="admin-crawler-section__title">任务状态</span>
                </div>
                <div className="admin-crawler-status-grid">
                  <CrawlerStatus label="抓取" status={selected.scanGenerationStatus} />
                  <CrawlerStatus label="封面" status={selected.thumbnailGenerationStatus} />
                  <CrawlerStatus label="预览视频" status={selected.previewGenerationStatus} />
                  <CrawlerStatus label="视频指纹" status={selected.fingerprintGenerationStatus} />
                </div>
                {selected.lastError && <div className="admin-detail-error">{selected.lastError}</div>}
              </div>
            )}
          </aside>
        </div>
      )}
    </section>
  );
}

function CrawlerMetric({ label, value, icon, tone }: { label: string; value: number; icon: ReactNode; tone?: "ok" | "info" | "error" }) {
  return (
    <div className={`admin-crawler-metric ${tone ? `is-${tone}` : ""}`}>
      <span className="admin-crawler-metric__icon">{icon}</span>
      <span>{label}</span>
      <strong>{value}</strong>
    </div>
  );
}

function CrawlerRow({
  crawler,
  active,
  running,
  stopping,
  onSelect,
  onRun,
  onStop,
}: {
  crawler: api.AdminCrawler;
  active: boolean;
  running: boolean;
  stopping: boolean;
  onSelect: () => void;
  onRun: () => void;
  onStop: () => void;
}) {
  return (
    <div className={`admin-crawler-row ${active ? "is-active" : ""}`}>
      <button type="button" className="admin-crawler-row__main" onClick={onSelect}>
        <span className="admin-crawler-row__brand">
          <SpiderIcon size={16} />
        </span>
        <span className="admin-crawler-row__title-wrap">
          <strong>{crawler.name}</strong>
          <span>{crawler.scriptPath ? "脚本已导入" : "未导入脚本"}</span>
        </span>
        <span className={`admin-status is-${crawler.status === "ok" ? "ok" : crawler.status === "error" ? "error" : "pending"}`}>
          {crawlerStatusLabel(crawler)}
        </span>
        <ChevronRight size={16} className="admin-crawler-row__chevron" />
      </button>
      <div className="admin-crawler-row__states">
        <CrawlerStateChip label="抓取" status={crawler.scanGenerationStatus} />
        <CrawlerStateChip label="封面" status={crawler.thumbnailGenerationStatus} />
        <CrawlerStateChip label="预览" status={crawler.previewGenerationStatus} />
        <CrawlerStateChip label="指纹" status={crawler.fingerprintGenerationStatus} />
      </div>
      <div className="admin-crawler-row__meta">
        <span><Gauge size={12} /> {crawler.targetNew || "10"} 条</span>
        <span><Clock size={12} /> {formatLastCrawl(crawler.lastCrawlAt)}</span>
      </div>
      <div className="admin-crawler-row__actions">
        <button className="admin-btn" type="button" onClick={onSelect}>
          <Settings2 size={13} /> 管理
        </button>
        <button className="admin-btn" type="button" onClick={onRun} disabled={running}>
          <Download size={13} /> {running ? "触发中..." : "立即抓取"}
        </button>
        <button className="admin-btn is-stop" type="button" onClick={onStop} disabled={stopping}>
          <CircleStop size={13} /> {stopping ? "停止中..." : "停止"}
        </button>
      </div>
    </div>
  );
}

function CrawlerStateChip({ label, status }: { label: string; status?: api.DriveGenerationStatus }) {
  const state = status?.state || "idle";
  return (
    <span className={`admin-crawler-state-chip is-${generationStateClass(state)}`}>
      {label} · {label === "抓取" && state === "scanning" ? "抓取中" : generationStateLabel(state)}
    </span>
  );
}

function CrawlerStatus({ label, status }: { label: string; status?: api.DriveGenerationStatus }) {
  const state = status?.state || "idle";
  const labelText = label === "抓取" && state === "scanning" ? "抓取中" : generationStateLabel(state);
  return (
    <div className="admin-gen-col">
      <div className="admin-gen-col__head">
        <span className="admin-gen-col__label">{label}</span>
        <span className={`admin-status admin-generation-state is-${generationStateClass(state)}`}>
          {labelText}
        </span>
      </div>
      {label === "抓取" && (
        <div className="admin-gen-col__counts admin-gen-col__counts--scan">
          <div className="admin-gen-col__count"><span>已抓取</span><strong>{status?.scannedCount ?? 0}</strong></div>
          <div className="admin-gen-col__count"><span>预计新增</span><strong>{status?.addedCount ?? 0}</strong></div>
        </div>
      )}
    </div>
  );
}

function CrawlerTestResult({ result }: { result: api.CrawlerDryRunResult }) {
  const item = result.items[0];
  const failure = crawlerTestFailure(result);
  const media = result.mediaCheck;
  const statusText = result.ok ? "测试通过" : "测试失败";

  return (
    <div className={`admin-crawler-test-result ${result.ok ? "is-ok" : "is-error"}`}>
      <div className="admin-crawler-test-result__head">
        <span className={`admin-status is-${result.ok ? "ok" : "error"}`}>{statusText}</span>
        <span>抓取到 {result.items.length} 条视频</span>
        {result.durationMs > 0 && <span>{Math.round(result.durationMs / 1000)} 秒</span>}
      </div>

      {failure && <div className="admin-crawler-test-result__error">{failure}</div>}

      {item && (
        <div className="admin-crawler-test-result__grid">
          <CrawlerTestField label="视频名" value={item.title} />
          <CrawlerTestField label="唯一标识" value={item.sourceId} />
          <CrawlerTestField label="视频直链" value={item.mediaUrl || item.mediaLocalFile} />
          <CrawlerTestField label="封面图" value={item.thumbnailUrl} />
          <CrawlerTestField label="详情页" value={item.detailUrl} />
        </div>
      )}

      {media && (
        <div className="admin-crawler-test-result__media">
          <span>直链校验</span>
          <strong>
            {media.ok ? "可访问" : "不可访问"}
            {media.status ? ` · HTTP ${media.status}` : ""}
            {media.contentType ? ` · ${media.contentType}` : ""}
            {media.contentLengthBytes ? ` · ${formatBytes(media.contentLengthBytes)}` : ""}
          </strong>
        </div>
      )}

      {result.log && result.log.length > 0 && (
        <details className="admin-crawler-test-result__log">
          <summary>脚本日志</summary>
          <pre>{result.log.join("\n")}</pre>
        </details>
      )}
    </div>
  );
}

function CrawlerTestField({ label, value }: { label: string; value?: string | number }) {
  if (value === undefined || value === "") return null;
  return (
    <div className="admin-crawler-test-result__field">
      <span>{label}</span>
      <strong>{value}</strong>
    </div>
  );
}

function crawlerTestFailure(result: api.CrawlerDryRunResult) {
  return result.error || result.mediaCheck?.error || "";
}

function crawlerStatusLabel(crawler: api.AdminCrawler) {
  if (crawler.status === "ok") return "已就绪";
  if (crawler.status === "error") return "错误";
  return "未连接";
}

function formatLastCrawl(ts?: number) {
  if (!ts) return "未抓取";
  return new Date(ts * 1000).toLocaleString("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  });
}

function formatBytes(bytes: number) {
  if (!Number.isFinite(bytes) || bytes <= 0) return "";
  if (bytes >= 1024 * 1024 * 1024) return `${(bytes / 1024 / 1024 / 1024).toFixed(1)} GB`;
  if (bytes >= 1024 * 1024) return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
  if (bytes >= 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${bytes} B`;
}
