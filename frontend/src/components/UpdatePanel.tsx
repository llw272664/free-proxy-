import { useCallback, useEffect, useRef, useState } from "react";
import * as api from "../api";
import type { ReleaseNotes, UpdateStatus } from "../types";
import { useUI } from "../store";
import { Card, Spinner } from "./ui";

// Phases of an update, in the order they happen. The download and the install
// are one job; the restart is not, because the server that would report it is
// the one being restarted.
type Phase = "idle" | "installing" | "restarting";

const PHASE_TEXT: Record<Phase, string> = {
  idle: "",
  installing: "正在下载并校验新版本…",
  restarting: "新版本已安装，服务正在重启…",
};

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

export function UpdatePanel({ onStatus }: { onStatus?: (s: UpdateStatus) => void }) {
  const push = useUI((s) => s.push);
  const [status, setStatus] = useState<UpdateStatus | null>(null);
  const [error, setError] = useState("");
  const [checking, setChecking] = useState(false);
  const [phase, setPhase] = useState<Phase>("idle");

  // Held in a ref so a caller passing an inline callback cannot make `load`
  // change identity on every render — which the effect below would read as a
  // reason to check again.
  const notify = useRef(onStatus);
  useEffect(() => { notify.current = onStatus; }, [onStatus]);

  const load = useCallback(async (refresh: boolean) => {
    setChecking(true);
    try {
      const s = await api.updateStatus(refresh);
      setStatus(s);
      setError("");
      notify.current?.(s);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setChecking(false);
    }
  }, []);

  useEffect(() => { load(false); }, [load]);

  async function runUpdate() {
    const target = status?.latest_version ?? "";
    setPhase("installing");
    try {
      const job = await api.startUpdate();
      await api.waitJob(job.id);
      setPhase("restarting");
      await waitForRestart(target);
      push("ok", `已更新到 ${target}，请重新登录`);
      load(true);
    } catch (e) {
      push("error", (e as Error).message);
    } finally {
      setPhase("idle");
    }
  }

  const busy = phase !== "idle";
  const pending = status?.pending ?? [];

  return (
    <Card
      title="版本更新"
      actions={
        <>
          <button className="btn" onClick={() => load(true)} disabled={checking || busy}>
            {checking ? <Spinner /> : "检查更新"}
          </button>
          {status?.update_available && (
            <button className="btn btn-primary" onClick={runUpdate} disabled={busy || !status.supported}>
              {busy ? <Spinner /> : `更新到 ${status.latest_version}`}
            </button>
          )}
        </>
      }
    >
      <div className="flex flex-wrap items-baseline gap-x-6 gap-y-1 text-sm">
        <span className="text-ink-3">当前版本 <span className="text-ink font-medium">{status?.current_version ?? "…"}</span></span>
        {status?.latest_version && (
          <span className="text-ink-3">
            最新版本{" "}
            <span className={`font-medium ${status.update_available ? "text-warn" : "text-ok"}`}>
              {status.latest_version}
            </span>
          </span>
        )}
        {status && !status.update_available && !error && (
          <span className="text-ok">已是最新版本</span>
        )}
      </div>

      {busy && (
        <div className="mt-3 flex items-center gap-2.5 text-sm text-ink-2">
          <Spinner />
          <span>{PHASE_TEXT[phase]}</span>
        </div>
      )}

      {error && <Note tone="danger">检查更新失败：{error}</Note>}

      {status && !status.supported && (
        <Note tone="warn">
          此环境无法自助更新：{status.unsupported_reason}
          <br />
          仍可在服务器上执行：<code>curl -fsSL https://raw.githubusercontent.com/MasterAlanLab/free-proxy/main/install.sh | sudo sh</code>
        </Note>
      )}

      {pending.length > 0 && (
        <div className="mt-4 pt-4 border-t border-rule">
          <div className="text-xs text-ink-3 mb-3">
            {pending.length > 1
              ? `更新后将一次性获得以下 ${pending.length} 个版本的改动：`
              : "本次更新的内容："}
          </div>
          <div className="grid gap-4">
            {pending.map((r) => <ReleaseCard key={r.version} release={r} />)}
          </div>
        </div>
      )}

      {status?.update_available && status.supported && (
        <p className="mt-4 text-xs text-ink-3">
          更新会下载对应架构的官方二进制、校验 SHA256、替换程序并重启服务。数据、节点、后台账号与管理路径都保留，
          代理会中断十几秒；服务重启后需要重新登录。
        </p>
      )}

      {status?.last_log && (
        <details className="mt-4">
          <summary className="text-xs text-ink-3 cursor-pointer">上次更新日志</summary>
          <pre className="mt-2 p-3 rounded-md border border-rule text-xs text-ink-2 overflow-x-auto whitespace-pre-wrap break-words">
            {status.last_log}
          </pre>
        </details>
      )}
    </Card>
  );
}

function ReleaseCard({ release }: { release: ReleaseNotes }) {
  const sections = release.sections ?? [];
  return (
    <section className="rounded-md border border-rule p-3.5">
      <header className="flex items-baseline justify-between gap-3 mb-2.5">
        <h3 className="text-sm font-semibold">{release.version}</h3>
        <div className="flex items-baseline gap-3 text-xs text-ink-3">
          <span>{formatDate(release.published_at)}</span>
          <a className="hover:text-ink underline underline-offset-2" href={release.url} target="_blank" rel="noreferrer">
            发布页
          </a>
        </div>
      </header>
      {sections.length === 0 ? (
        <div className="text-sm text-ink-3">这个版本没有提供更新说明</div>
      ) : (
        <div className="grid gap-2.5">
          {sections.map((s) => (
            <div key={s.title} className="grid sm:grid-cols-[5.5rem_1fr] gap-x-3 gap-y-1">
              <div className="text-xs text-ink-3 sm:text-right sm:pt-0.5">{s.title}</div>
              <ul className="grid gap-1">
                {s.items.map((item, i) => (
                  <li key={i} className="text-sm text-ink-2 break-words">{item}</li>
                ))}
              </ul>
            </div>
          ))}
        </div>
      )}
    </section>
  );
}

function Note({ tone, children }: { tone: "warn" | "danger"; children: React.ReactNode }) {
  const color = tone === "danger" ? "text-danger" : "text-warn";
  return <div className={`mt-3 text-sm ${color} break-words`}>{children}</div>;
}

// waitForRestart watches for the service coming back as the new version. Two
// answers count as success: the version this console reports changes, or the
// session stops being accepted — sessions live in memory, so losing one is
// proof the process behind it was replaced.
async function waitForRestart(target: string) {
  const deadline = Date.now() + 5 * 60 * 1000;
  while (Date.now() < deadline) {
    await sleep(3000);
    try {
      const s = await api.systemStatus();
      if (!target || s.version === target) return;
    } catch (e) {
      if (e instanceof api.ApiError && e.status === 401) return;
      // Anything else is the server being down mid-restart; keep waiting.
    }
  }
  throw new Error("服务在 5 分钟内没有回到在线状态，请在服务器上执行 systemctl status free-proxy 查看");
}

function formatDate(iso: string) {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? "" : d.toLocaleDateString("zh-CN");
}
