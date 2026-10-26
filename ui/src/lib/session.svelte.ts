import { ApiError, message, request, type Job, type WorkerStatus } from "./api";

class Session {
  token = $state("");
  jobs = $state.raw<Job[]>([]);
  workers = $state.raw<WorkerStatus[]>([]);
  error = $state("");
  notice = $state("");
  updated = $state<Date | null>(null);
  live = $state(true);
  #timer: ReturnType<typeof setInterval> | undefined;
  #noticeTimer: ReturnType<typeof setTimeout> | undefined;
  #refreshing = false;

  get connected() {
    return this.token !== "";
  }

  async connect(token: string) {
    try {
      await request(token, "workers");
    } catch (error) {
      throw new Error(error instanceof ApiError && error.status === 401 ? "That token was rejected." : message(error));
    }
    this.token = token;
    this.error = "";
    await this.refresh();
    clearInterval(this.#timer);
    if (!this.connected) return;
    this.#timer = setInterval(() => {
      if (this.live && !document.hidden) void this.refresh();
    }, 2000);
  }

  disconnect(reason = "") {
    clearInterval(this.#timer);
    this.token = "";
    this.jobs = [];
    this.workers = [];
    this.updated = null;
    this.error = reason;
  }

  async call(path: string, method = "GET", body?: unknown): Promise<Response> {
    const token = this.token;
    try {
      const response = await request(token, path, method, body);
      if (token !== this.token) throw new Error("Session changed; response discarded.");
      return response;
    } catch (error) {
      if (error instanceof ApiError && error.status === 401 && token === this.token) {
        this.disconnect("The server rejected the token; connect again.");
      }
      throw error;
    }
  }

  async refresh() {
    if (!this.connected || this.#refreshing) return;
    this.#refreshing = true;
    try {
      const [jobs, workers] = await Promise.all([this.call("jobs").then((r) => r.json()), this.call("workers").then((r) => r.json())]);
      this.jobs = jobs ?? [];
      this.workers = workers ?? [];
      this.updated = new Date();
      this.error = "";
    } catch (error) {
      if (this.connected) this.error = message(error);
    } finally {
      this.#refreshing = false;
    }
  }

  flash(text: string) {
    this.notice = text;
    clearTimeout(this.#noticeTimer);
    this.#noticeTimer = setTimeout(() => (this.notice = ""), 4000);
  }

  async cancel(kind: "jobs" | "workflows", id: string) {
    await this.call(`${kind}/${encodeURIComponent(id)}/cancel`, "POST");
    this.flash(`Cancellation requested for ${id}.`);
    await this.refresh();
  }
}

export const session = new Session();
