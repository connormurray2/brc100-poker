// Pure bookkeeping for the MessageBox latency run: which sequence numbers were lost or duplicated,
// and the latency summary. No network, no clock: the harness feeds it plain numbers.

export interface Reconciliation {
  /** Sequence numbers sent but never received. */
  lost: number[];
  /** Sequence numbers received more than once (each listed once, ascending). */
  duplicated: number[];
  /** Sequence numbers received that were never sent. */
  unexpected: number[];
  ok: boolean;
}

export function reconcile(sent: readonly number[], received: readonly number[]): Reconciliation {
  const sentSet = new Set(sent);
  const counts = new Map<number, number>();
  for (const r of received) counts.set(r, (counts.get(r) ?? 0) + 1);
  const asc = (a: number, b: number) => a - b;
  const lost = [...sentSet].filter((s) => !counts.has(s)).sort(asc);
  const duplicated = [...counts].filter(([, n]) => n > 1).map(([s]) => s).sort(asc);
  const unexpected = [...counts.keys()].filter((s) => !sentSet.has(s)).sort(asc);
  return { lost, duplicated, unexpected, ok: !lost.length && !duplicated.length && !unexpected.length };
}

/** Nearest-rank percentile (p in 0..100) of a non-empty list. */
export function percentile(values: readonly number[], p: number): number {
  if (values.length === 0) throw new Error('percentile of an empty list');
  if (!(p >= 0 && p <= 100)) throw new Error('p must be within 0..100');
  const sorted = [...values].sort((a, b) => a - b);
  const rank = Math.max(1, Math.ceil((p / 100) * sorted.length));
  return sorted[rank - 1]!;
}

/** Median: the mean of the two middle values for an even count. */
export function median(values: readonly number[]): number {
  if (values.length === 0) throw new Error('median of an empty list');
  const sorted = [...values].sort((a, b) => a - b);
  const mid = sorted.length >> 1;
  return sorted.length % 2 ? sorted[mid]! : (sorted[mid - 1]! + sorted[mid]!) / 2;
}

export interface LatencySummary {
  n: number;
  min: number;
  median: number;
  p95: number;
  max: number;
  mean: number;
}

export function summarise(values: readonly number[]): LatencySummary {
  if (values.length === 0) throw new Error('summary of an empty list');
  return {
    n: values.length,
    min: Math.min(...values),
    median: median(values),
    p95: percentile(values, 95),
    max: Math.max(...values),
    mean: values.reduce((a, b) => a + b, 0) / values.length,
  };
}

export interface LiveSample { seq: number; rttMs: number }

export interface LiveClassification {
  /** Samples whose ping and echo both travelled the live socket: the only ones that may enter the RTT stats. */
  live: LiveSample[];
  /** Sequence numbers (ascending, unique) where any send fell back to HTTP. */
  fellBack: number[];
}

/**
 * sendLiveMessage falls back to HTTP silently, and a fallback waits out the library's 10 s ack timeout before
 * it sends, so its timing is not a live latency. Any sample touched by a fallback is excluded.
 */
export function classifyLiveSamples(samples: readonly LiveSample[], fellBackSeqs: Iterable<number>): LiveClassification {
  const fb = new Set(fellBackSeqs);
  return {
    live: samples.filter((s) => !fb.has(s.seq)),
    fellBack: [...fb].sort((a, b) => a - b),
  };
}

/** The live result is FAIL if any live send fell back (`count` includes sends that could not be attributed to a seq). */
export function liveFallbackVerdict(count: number): { ok: boolean; reason?: string } {
  return count === 0 ? { ok: true } : { ok: false, reason: `${count} live send(s) fell back to HTTP` };
}
