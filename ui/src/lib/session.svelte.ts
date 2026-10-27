import { ApiError, message, request, type Job, type JobSummary, type WorkerStatus } from "./api";

class Session {
  token = $state("");
  jobs = $state.raw<JobSummary[]>([]);
  // The list holds summaries; the job open in the detail view is fetched whole.
  selected = $state<string | null>(null);
  detail = $state.raw<Job | null>(null);
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
    this.selected = null;
    this.detail = null;
    this.workers = [];
    this.updated = null;
    this.error = reason;
  }

  async call(path: string, method = "GET"): Promise<Response> {
    const token = this.token;
    try {
      const response = await request(token, path, method);
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
      const [jobs, workers] = await Promise.all([this.call("jobs").then((r) => r.json()), this.call("workers").then((r) => r.json()), this.#loadDetail()]);
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

  async select(id: string | null) {
    this.selected = id;
    this.detail = null;
    try {
      await this.#loadDetail();
    } catch (error) {
      if (this.connected) this.error = message(error);
    }
  }

  async #loadDetail() {
    const id = this.selected;
    if (!id) return;
    const job: Job = await this.call(`jobs/${encodeURIComponent(id)}`).then((r) => r.json());
    if (this.selected === id) this.detail = job;
  }

  flash(text: string) {
    this.notice = text;
    clearTimeout(this.#noticeTimer);
    this.#noticeTimer = setTimeout(() => (this.notice = ""), 4000);
  }

  async cancel(id: string) {
    await this.call(`jobs/${encodeURIComponent(id)}/cancel`, "POST");
    const out = this.jobs.find((j) => j.id === id)?.out ?? (this.detail?.id === id ? this.detail.out : undefined);
    this.flash(`Cancellation requested for ${out ?? id}.`);
    await this.refresh();
  }
}

export const session = new Session();
