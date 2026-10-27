import type { JobState, JobSummary, Resources } from "./api";

export const terminal: JobState[] = ["succeeded", "failed", "cancelled", "skipped"];
export const isActive = (state: JobState) => !terminal.includes(state);

export const filters: { id: string; label: string; match: (job: JobSummary) => boolean }[] = [
  { id: "all", label: "All", match: () => true },
  { id: "active", label: "Active", match: (j) => isActive(j.state) },
  { id: "succeeded", label: "Succeeded", match: (j) => j.state === "succeeded" },
  { id: "failed", label: "Failed", match: (j) => j.state === "failed" },
  { id: "stopped", label: "Cancelled / skipped", match: (j) => j.state === "cancelled" || j.state === "skipped" },
];

const neutral = { badge: "bg-zinc-100 text-zinc-700 ring-zinc-300 dark:bg-zinc-800 dark:text-zinc-300 dark:ring-zinc-700", dot: "fill-zinc-400" };
const transfer = { badge: "bg-sky-50 text-sky-700 ring-sky-200 dark:bg-sky-950 dark:text-sky-300 dark:ring-sky-900", dot: "fill-sky-500" };
const good = { badge: "bg-emerald-50 text-emerald-700 ring-emerald-200 dark:bg-emerald-950 dark:text-emerald-300 dark:ring-emerald-900", dot: "fill-emerald-500" };
const bad = { badge: "bg-rose-50 text-rose-700 ring-rose-200 dark:bg-rose-950 dark:text-rose-300 dark:ring-rose-900", dot: "fill-rose-500" };
const stopped = { badge: "bg-zinc-100 text-zinc-500 ring-zinc-200 dark:bg-zinc-900 dark:text-zinc-500 dark:ring-zinc-800", dot: "fill-zinc-300 dark:fill-zinc-600" };

export const palette: Record<string, { badge: string; dot: string }> = {
  queued: neutral,
  dispatching: transfer,
  collecting: transfer,
  delivering: transfer,
  running: { badge: "bg-blue-50 text-blue-700 ring-blue-200 dark:bg-blue-950 dark:text-blue-300 dark:ring-blue-900", dot: "fill-blue-500" },
  blocked: { badge: "bg-amber-50 text-amber-800 ring-amber-200 dark:bg-amber-950 dark:text-amber-300 dark:ring-amber-900", dot: "fill-amber-500" },
  succeeded: good,
  failed: bad,
  cancelled: stopped,
  skipped: stopped,
  connected: good,
  offline: bad,
};

export const colors = (state: string) => palette[state] ?? neutral;

export function shortOut(out: string): string {
  const parts = out.split("/").filter(Boolean);
  return parts.length > 3 ? `…/${parts.slice(-3).join("/")}` : out;
}

export const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? "" : "s"}`;

export function relative(time: string | Date | null | undefined): string {
  if (!time) return "—";
  const date = new Date(time);
  if (date.getFullYear() < 2000) return "never";
  const seconds = Math.round((Date.now() - date.getTime()) / 1000);
  if (seconds < 5) return "just now";
  if (seconds < 60) return `${seconds}s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`;
  return date.toLocaleDateString();
}

export function duration(start?: string, end?: string): string {
  if (!start || !end) return "—";
  const ms = new Date(end).getTime() - new Date(start).getTime();
  if (!(ms >= 0) || new Date(start).getFullYear() < 2000) return "—";
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`;
  return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
}

export function memory(mib: number): string {
  return mib >= 1024 ? `${(mib / 1024).toFixed(mib % 1024 === 0 ? 0 : 1)} GiB` : `${mib} MiB`;
}

export function resources({ cores, memory_mib, gpus }: Resources): string {
  return [plural(cores, "core"), memory(memory_mib), gpus ? plural(gpus, "GPU") : ""].filter(Boolean).join(" · ");
}

export function shortImage(image: string): string {
  return image.startsWith("sha256:") ? `sha256:${image.slice(7, 19)}` : image;
}
