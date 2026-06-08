import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const drivesPageSource = readFileSync(
  new URL("../src/admin/DrivesPage.tsx", import.meta.url),
  "utf8"
);
const driveComponentsSource = readFileSync(
  new URL("../src/admin/drive/DriveComponents.tsx", import.meta.url),
  "utf8"
);
const spider91UploadTargetSource = readFileSync(
  new URL("../src/admin/drive/Spider91UploadTargetField.tsx", import.meta.url),
  "utf8"
);
const driveFormSource = readFileSync(
  new URL("../src/admin/drive/DriveForm.tsx", import.meta.url),
  "utf8"
);
const adminCss = readFileSync(
  new URL("../src/styles/admin.css", import.meta.url),
  "utf8"
);
const apiSource = readFileSync(
  new URL("../src/admin/api.ts", import.meta.url),
  "utf8"
);
const constantsSource = readFileSync(
  new URL("../src/admin/drive/constants.ts", import.meta.url),
  "utf8"
);

const combinedSource = drivesPageSource + "\n" + driveFormSource + "\n" + constantsSource + "\n" + spider91UploadTargetSource;

function driveTypeOptions() {
  const match = /const DRIVE_OPTIONS:\s*DriveOption\[]\s*=\s*\[([\s\S]*?)\];/.exec(
    driveFormSource
  );
  assert.ok(match, "drive option card list should be present");
  return Array.from(
    match[1].matchAll(/\{\s*kind:\s*"([^"]+)",\s*label:\s*"([^"]+)"/g),
    (option) => ({ value: option[1], label: option[2] })
  );
}

function assertDriveTypeOption(value: string, label: string) {
  assert.ok(
    driveTypeOptions().some((option) => option.value === value && option.label === label),
    `${value} drive type option should be present`
  );
}

test("spider91 drive form does not expose advanced crawler credentials", () => {
  assert.match(combinedSource, /key: "proxy"/);
  assert.match(combinedSource, /label: "代理地址（可选）"/);
  assert.match(combinedSource, /支持 http:\/\/、https:\/\/、socks5:\/\/、socks5h:\/\/代理/);
  assert.doesNotMatch(combinedSource, /target_new/);
  assert.doesNotMatch(combinedSource, /crawl_hour/);
  assert.doesNotMatch(combinedSource, /python_path/);
  assert.doesNotMatch(combinedSource, /script_path/);
});

test("spider91 upload target uses explicit local-save option instead of auto target", () => {
  assert.match(combinedSource, /本地保存，不上传/);
  assert.match(
    combinedSource,
    /d\.kind === "pikpak" \|\| d\.kind === "p115" \|\| d\.kind === "p123" \|\| d\.kind === "onedrive"/
  );
  assert.doesNotMatch(combinedSource, /自动：唯一/);
  assert.doesNotMatch(combinedSource, /自动模式/);
  assert.doesNotMatch(combinedSource, /较早的视频会上传到该云盘根目录下的 91 Spider 文件夹/);
});

test("spider91 upload target select uses an aligned custom arrow", () => {
  assert.match(spider91UploadTargetSource, /className="admin-form-select-wrap"/);
  assert.match(spider91UploadTargetSource, /className="admin-form-select"/);
  assert.match(spider91UploadTargetSource, /className="admin-form-select__icon"/);
  assert.match(adminCss, /\.admin-form__row \.admin-form-select\s*\{[^}]*appearance\s*:\s*none/s);
  assert.match(
    adminCss,
    /\.admin-form-select__icon\s*\{[^}]*top\s*:\s*50%[^}]*right\s*:\s*12px[^}]*transform\s*:\s*translateY\(-50%\)/s
  );
});

test("drive form hides root directory id for localstorage and spider91", () => {
  assert.match(combinedSource, /<label[^>]*>根目录 ID<\/label>/);
  assert.match(
    combinedSource,
    /usesRootDirectoryID\(kind:\s*Kind\):\s*boolean\s*\{\s*return kind !== "localstorage" && kind !== "spider91";\s*\}/
  );
  assert.match(combinedSource, /\{usesRootDirectoryID\(form\.kind\) && \(/);
  assert.match(combinedSource, /\{usesRootDirectoryID\(d\.kind\) && \(/);
  assert.match(combinedSource, /placeholder=\{rootIdPlaceholder\(form\.kind\)\}/);
  assert.doesNotMatch(combinedSource, /扫描起点目录 ID/);
  assert.doesNotMatch(combinedSource, /set\("scanRootId"/);
});

test("onedrive drive form only exposes required default-app fields", () => {
  const match =
    /case "onedrive":\s*return \[([\s\S]*?)\];\s*case "googledrive":/.exec(
      combinedSource
    );
  assert.ok(match, "onedrive credential field block should be present");
  const fields = match[1];

  assert.match(fields, /key: "refresh_token"/);
  assert.doesNotMatch(fields, /key: "access_token"/);
  assert.doesNotMatch(fields, /key: "api_url_address"/);
  assert.doesNotMatch(fields, /key: "region"/);
  assert.doesNotMatch(fields, /key: "is_sharepoint"/);
  assert.doesNotMatch(fields, /key: "site_id"/);
});

test("googledrive drive form only exposes refresh token", () => {
  assertDriveTypeOption("googledrive", "Google Drive");

  const match =
    /case "googledrive":\s*return \[([\s\S]*?)\];\s*case "localstorage":/.exec(
      combinedSource
    );
  assert.ok(match, "googledrive credential field block should be present");
  const fields = match[1];

  assert.match(fields, /key: "refresh_token"/);
  assert.doesNotMatch(fields, /key: "access_token"/);
  assert.doesNotMatch(fields, /key: "api_url_address"/);
  assert.doesNotMatch(fields, /key: "client_id"/);
  assert.doesNotMatch(fields, /key: "client_secret"/);
});

test("pikpak drive form only exposes account login fields", () => {
  const match =
    /case "pikpak":\s*return \[([\s\S]*?)\];\s*case "wopan":/.exec(
      combinedSource
    );
  assert.ok(match, "pikpak credential field block should be present");
  const fields = match[1];

  assert.match(fields, /key: "username"/);
  assert.match(fields, /key: "password"/);
  assert.doesNotMatch(fields, /key: "platform"/);
  assert.doesNotMatch(fields, /key: "refresh_token"/);
  assert.doesNotMatch(fields, /key: "captcha_token"/);
  assert.doesNotMatch(fields, /key: "device_id"/);
  assert.doesNotMatch(fields, /key: "disable_media_link"/);
});

test("localstorage drive form asks for a server directory path", () => {
  assertDriveTypeOption("localstorage", "本地存储");

  const match =
    /case "localstorage":\s*return \[([\s\S]*?)\];\s*case "spider91":/.exec(
      combinedSource
    );
  assert.ok(match, "localstorage credential field block should be present");
  const fields = match[1];

  assert.match(fields, /key: "path"/);
  assert.match(fields, /label: "本地目录路径"/);
  assert.match(combinedSource, /if \(kind === "localstorage"\) return "\/"/);
  assert.match(combinedSource, /kind !== "localstorage" && kind !== "spider91"/);
});

test("drive type selector keeps primary source order", () => {
  assert.deepEqual(driveTypeOptions(), [
    { value: "p115", label: "115 网盘" },
    { value: "p123", label: "123 云盘" },
    { value: "pikpak", label: "PikPak" },
    { value: "onedrive", label: "OneDrive" },
    { value: "googledrive", label: "Google Drive" },
    { value: "localstorage", label: "本地存储" },
    { value: "spider91", label: "91 爬虫" },
    { value: "quark", label: "夸克网盘" },
    { value: "wopan", label: "联通沃盘" },
  ]);
});

test("drive management exposes stop task controls", () => {
  assert.match(apiSource, /stopDriveTasks/);
  assert.match(apiSource, /\/drives\/\$\{encodeURIComponent\(id\)\}\/tasks\/stop/);
  assert.match(apiSource, /stopAllTasks/);
  assert.match(apiSource, /"\/tasks\/stop"/);
  assert.match(drivesPageSource, /is-stop/);
  assert.match(drivesPageSource, /停止所有任务/);
  assert.match(drivesPageSource, /停止所有网盘任务/);
});

test("drive rescan reports busy storage tasks instead of queueing duplicates", () => {
  assert.match(apiSource, /accepted:\s*boolean;\s*message\?:\s*string/);
  assert.match(apiSource, /scanGenerationStatus\?: DriveGenerationStatus/);
  assert.match(drivesPageSource, /当前存储有正在进行的任务，请稍后重试/);
  assert.match(drivesPageSource, /function isDriveBusy\(d: api\.AdminDrive\)/);
  assert.match(drivesPageSource, /d\.scanGenerationStatus/);
  assert.match(drivesPageSource, /status\?\.state \|\| "idle"/);
  assert.match(drivesPageSource, /scanningDriveIdsRef\.current\.has\(d\.id\)/);
  assert.match(drivesPageSource, /if \(!resp\.accepted\)/);
  assert.doesNotMatch(drivesPageSource, /disabled=\{!!scanningDriveId\}/);
});

test("nightly scan duplicate trigger uses full-scan busy message", () => {
  assert.match(apiSource, /status:\s*NightlyJobStatus;\s*message\?:\s*string/);
  assert.match(drivesPageSource, /当前有全量扫描任务正在进行，请稍后重试/);
  assert.match(drivesPageSource, /resp\.message \|\| NIGHTLY_BUSY_MESSAGE/);
  assert.match(constantsSource, /当前有全量扫描任务正在进行，请稍后重试/);
});

test("drive generation panel shows scan or crawler status first", () => {
  assert.match(driveComponentsSource, /label=\{d\.kind === "spider91" \? "抓取" : "扫盘"\}/);
  assert.match(driveComponentsSource, /status=\{d\.scanGenerationStatus\}/);
  assert.match(driveComponentsSource, /showCounts=\{false\}/);
  assert.match(driveComponentsSource, /label === "抓取" && state === "scanning" \? "抓取中"/);
  assert.match(driveComponentsSource, /status\?\.scannedCount/);
  assert.match(driveComponentsSource, /预计新增/);
  assert.match(apiSource, /scannedCount:\s*number/);
  assert.match(apiSource, /addedCount:\s*number/);
  assert.match(constantsSource, /if \(state === "scanning"\) return "扫盘中"/);
});

test("drive detail selection is stored in the URL history", () => {
  assert.match(drivesPageSource, /useSearchParams/);
  assert.match(drivesPageSource, /searchParams\.get\("drive"\)/);
  assert.match(drivesPageSource, /function openDriveDetail\(id: string\)/);
  assert.match(drivesPageSource, /next\.set\("drive", id\)/);
  assert.match(drivesPageSource, /function closeDriveDetail/);
  assert.match(drivesPageSource, /next\.delete\("drive"\)/);
  assert.doesNotMatch(drivesPageSource, /setSelectedDriveId/);
});

test("drive discard confirmation matches delete confirmation modal styling", () => {
  const discardModals = Array.from(
    drivesPageSource.matchAll(/<ConfirmModal[\s\S]*?title="放弃未保存更改"[\s\S]*?\/>/g),
    (match) => match[0]
  );

  assert.equal(discardModals.length, 2);
  for (const modal of discardModals) {
    assert.match(modal, /danger/);
    assert.match(modal, /centerMessage/);
    assert.match(modal, /modalClassName="admin-modal--delete-confirm"/);
  }
});

test("new drive type selection alone is not treated as unsaved config", () => {
  assert.match(
    drivesPageSource,
    /const formDirty = form\.id\s*\?\s*!sameForm\(form, initialForm\)\s*:\s*hasCreateFormChanges\(form, initialForm\);/
  );
  assert.match(drivesPageSource, /function handleCreateFormChange\(nextForm: FormState\)/);
  assert.match(
    drivesPageSource,
    /if \(!nextForm\.id && !hasCreateFormChanges\(nextForm, initialForm\)\) \{\s*setInitialForm\(nextForm\);/
  );
  assert.match(drivesPageSource, /onChange=\{handleCreateFormChange\}/);

  const match = /function hasCreateFormChanges\(form: FormState, initial: FormState\): boolean \{([\s\S]*?)\n\}/.exec(
    drivesPageSource
  );
  assert.ok(match, "create form dirty helper should be present");
  const helper = match[1];

  assert.match(helper, /form\.name\.trim\(\) !== ""/);
  assert.match(helper, /form\.rootId\.trim\(\) !== ""/);
  assert.match(helper, /form\.spider91UploadDriveId !== initial\.spider91UploadDriveId/);
  assert.match(helper, /Object\.values\(form\.creds\)\.some/);
  assert.doesNotMatch(helper, /form\.kind/);
});

test("drive generation actions can resume pending work after stop", () => {
  assert.match(driveComponentsSource, /thumbnailPendingCount/);
  assert.match(driveComponentsSource, /teaserPendingCount/);
  assert.match(driveComponentsSource, /fingerprintPendingCount/);
  assert.match(driveComponentsSource, /继续生成封面/);
  assert.match(driveComponentsSource, /继续生成预览视频/);
  assert.match(driveComponentsSource, /继续生成指纹/);
});

test("drive cards label fingerprint count as video fingerprint count", () => {
  assert.match(driveComponentsSource, /视频指纹数 \(就绪\/失败\)/);
  assert.doesNotMatch(driveComponentsSource, />指纹数 \(就绪\/失败\)</);
});
