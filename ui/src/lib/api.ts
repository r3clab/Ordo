export type JobState = "queued" | "dispatching" | "running" | "blocked" | "collecting" | "succeeded" | "failed" | "cancelled" | "skipped";

export interface Resources {
  cores: number;
  memory_mib: number;
  gpus?: number;
}

export interface Allocation {
  cpus: number[];
  memory_mib: number;
  gpus?: string[];
}

export interface GPU {
  uuid: string;
  index: number;
  model: string;
  memory_mib: number;
  driver: string;
}

export interface Hardware {
  hostname: string;
  arch: string;
  cpu_model: string;
  cores: number[][] | null;
  memory_mib: number;
  gpus?: GPU[];
}

export interface Spec {
  image: string;
  args?: string[];
  resources: Resources;
  inputs?: Record<string, string>;
  timeout_seconds?: number;
  match?: Record<string, string | number>;
}

export interface TaskRecord {
  container_id?: string;
  state: string;
  started_at: string;
  finished_at: string;
  exit_code?: number;
  oom_killed: boolean;
  error?: string;
}

export interface RemoteAttempt {
  id: string;
  state: string;
  error?: string;
  allocation: Allocation;
  created_at: string;
  updated_at: string;
  result?: TaskRecord;
  stop_requested?: string;
}

export interface Attempt {
  id: string;
  worker: string;
  agent_id: string;
  remote?: RemoteAttempt;
  result_dir?: string;
  failure?: string;
}

export interface Job {
  id: string;
  spec: Spec;
  state: JobState;
  error?: string;
  created_at: string;
  attempts?: Attempt[];
  workflow?: string;
  depends_on?: string[];
  from?: Record<string, string>;
  parameters?: Record<string, string>;
  retry?: { task?: number; infrastructure?: number };
  cancel_requested?: boolean;
}

export interface WorkerStatus {
  host: string;
  connected: boolean;
  last_seen: string;
  error?: string;
  info?: { id: string; hardware: Hardware; held: Allocation[] | null; error?: string };
}

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
  }
}

export async function request(token: string, path: string, method = "GET", body?: unknown): Promise<Response> {
  const response = await fetch(`/v1/${path}`, {
    method,
    credentials: "omit",
    headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!response.ok) {
    throw new ApiError(response.status, (await response.text()).trim() || response.statusText);
  }
  return response;
}

export function message(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
