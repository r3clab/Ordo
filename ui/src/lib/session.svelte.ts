import { message, request, type Job, type JobSummary, type WorkerStatus } from "./api";

class Session {
  jobs = $state.raw<JobSummary[]>([]);
  // The list holds summaries; the job open in the detail view is fetched whole.
  selected = $state<string | null>(null);
  detail = $state.raw<Job | null>(null);
  workers = $state.raw<WorkerStatus[]>([]);
  error = $state("");
  notice = $state("");
  updated = $state<Date | null>(null);
  live = $state(true);
  #noticeTimer: ReturnType<typeof setTimeout> | undefined;
  #refreshing = false;
  #jobsETag = "";

  start() {
    void this.refresh();
    setInterval(() => {
      if (this.live && !document.hidden) void this.refresh();
    }, 2000);
  }

  async refresh() {
    if (this.#refreshing) return;
    this.#refreshing = true;
    try {
      const [jobs, workers] = await Promise.all([
        request("jobs", "GET", this.#jobsETag ? { "If-None-Match": this.#jobsETag } : undefined),
        request("workers").then((r) => r.json()),
        this.#loadDetail(),
      ]);
      if (jobs.status !== 304) {
        this.jobs = (await jobs.json()) ?? [];
        this.#jobsETag = jobs.headers.get("ETag") ?? "";
      }
      this.workers = workers ?? [];
      this.updated = new Date();
      this.error = "";
    } catch (error) {
      this.error = message(error);
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
      this.error = message(error);
    }
  }

  async #loadDetail() {
    const id = this.selected;
    if (!id) return;
    const job: Job = await request(`jobs/${encodeURIComponent(id)}`).then((r) => r.json());
    if (this.selected === id) this.detail = job;
  }

  flash(text: string) {
    this.notice = text;
    clearTimeout(this.#noticeTimer);
    this.#noticeTimer = setTimeout(() => (this.notice = ""), 4000);
  }

  async cancel(id: string) {
    await request(`jobs/${encodeURIComponent(id)}/cancel`, "POST");
    const out = this.jobs.find((j) => j.id === id)?.out ?? (this.detail?.id === id ? this.detail.out : undefined);
    this.flash(`Cancellation requested for ${out ?? id}.`);
    await this.refresh();
  }
}

export const session = new Session();
