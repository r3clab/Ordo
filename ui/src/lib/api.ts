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
  tags?: string[];
}

export interface Result {
  exit_code: number;
  oom_killed?: boolean;
  started_at: string;
  finished_at: string;
  outputs_sha256: string;
}

export interface Attempt {
  id: string;
  worker: string;
  machine_id: string;
  allocation: Allocation;
  created_at: string;
  container_id?: string;
  stop_requested?: string;
  stop_reason?: string;
  result?: Result;
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
  retry?: { task?: number; infrastructure?: number };
  cancel_requested?: boolean;
}

export interface JobSummary {
  id: string;
  state: JobState;
  workflow?: string;
  image: string;
  resources: Resources;
  worker?: string;
  attempts?: number;
  created_at: string;
  error?: string;
  depends_on?: string[];
  cancel_requested?: boolean;
}

export interface WorkerStatus {
  host: string;
  connected: boolean;
  last_seen: string;
  error?: string;
  info?: { boot_id: string; hardware: Hardware; held: Allocation[] | null; error?: string };
  tags?: string[];
}

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
  }
}

export async function request(token: string, path: string, method = "GET"): Promise<Response> {
  const response = await fetch(`/v1/${path}`, {
    method,
    credentials: "omit",
    headers: { Authorization: `Bearer ${token}` },
  });
  if (!response.ok) {
    throw new ApiError(response.status, (await response.text()).trim() || response.statusText);
  }
  return response;
}

export function message(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
