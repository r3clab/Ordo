export type JobState = "queued" | "dispatching" | "running" | "blocked" | "collecting" | "delivering" | "succeeded" | "failed" | "cancelled" | "skipped";

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
  cpus: number[] | null;
  memory_mib: number;
  gpus?: GPU[];
}

export interface Spec {
  image: string;
  entrypoint?: string[];
  args?: string[];
  workdir?: string;
  resources: Resources;
  inputs?: Record<string, string>;
  env?: Record<string, string>;
  network?: string;
  cap_add?: string[];
  secrets?: string[];
  services?: string[];
  timeout_seconds?: number;
  tags?: string[];
}

export interface Stage {
  name?: string;
  spec: Spec;
  archive: string;
  image_origin?: { installable?: string; output?: string; reference?: string };
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
  step: number;
  worker: string;
  machine_id: string;
  boot_id: string;
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
  out: string;
  steps: Stage[];
  step: number;
  ready_at?: string;
  state: JobState;
  error?: string;
  created_at: string;
  attempts?: Attempt[];
  submission_key?: string;
  retry?: { task?: number; infrastructure?: number };
  cancel_requested?: boolean;
  stop_requested?: boolean;
  stop_reason?: string;
  inputs?: Record<string, string>;
  waiting?: string[];
  after?: string[];
  pending?: Record<string, string>;
}

export interface JobSummary {
  id: string;
  out: string;
  state: JobState;
  step: number;
  steps: number;
  image: string;
  resources: Resources;
  worker?: string;
  attempts?: number;
  created_at: string;
  error?: string;
  cancel_requested?: boolean;
  after?: string[];
  pending?: Record<string, string>;
}

export interface WorkerStatus {
  host: string;
  connected: boolean;
  last_seen: string;
  error?: string;
  info?: { boot_id: string; hardware: Hardware; held: Allocation[] | null; error?: string };
  tags?: string[];
}

export async function request(path: string, method = "GET", headers?: HeadersInit): Promise<Response> {
  const response = await fetch(`/v1/${path}`, { method, headers, credentials: "omit" });
  if (!response.ok && response.status !== 304) {
    throw new Error((await response.text()).trim() || response.statusText);
  }
  return response;
}

export function message(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
