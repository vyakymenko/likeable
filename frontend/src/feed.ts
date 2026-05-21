import { LIKEABLE_NOTIFICATION_END, LIKEABLE_NOTIFICATION_START } from './config';
import type { AgentActivity, AgentMessage, Feed, FeedRow, Message, MessageAttachment, NotificationFeedRow, NotificationTiming } from './domain';

const TURN_TIME_SLOP_MS = 5000;
const RECENT_UNANSWERED_TURN_MS = 30 * 60_000;
const RECENT_ACTIVITY_WORKING_MS = 15 * 60_000;
const AGENT_WAITING_HINT_MS = 10_000;
const AGENT_STALLED_HINT_MS = 90_000;
const AGENT_STALE_WARNING_MAX_MS = 24 * 60 * 60_000;
const PROJECT_UPDATE_FALLBACK_GRACE_MS = 15_000;

export type AgentResponseDelayStatus = { phase: 'waiting' | 'stalled'; active: boolean; tone?: 'warning' };

export function feedRows(feed: Feed | null, now = Date.now()): FeedRow[] {
  if (!feed) return [];
  const rows: FeedRow[] = [];
  for (const msg of feed.localMessages ?? []) {
    if (msg.role !== 'user') continue;
    const normalized = normalizeLocalMessage(msg);
    normalized.body = stripInternalLikeableMarkers(normalized.body);
    if (!normalized.body.trim() && normalized.attachments.length === 0) continue;
    rows.push({ kind: 'user', id: msg.id, role: 'user', body: normalized.body, attachments: normalized.attachments, time: msg.createdAt });
  }
  const notificationRows = notificationFeedRows(feed);
  rows.push(...notificationRows);
  const projectUpdateRow = projectUpdateNotificationRow(feed, notificationRows);
  if (projectUpdateRow) rows.push(projectUpdateRow);
  return applyNotificationTimings(rows.sort(compareFeedRows), feed.notificationTimings, now);
}

function normalizeLocalMessage(msg: Message): { body: string; attachments: MessageAttachment[] } {
  const attachments = [...(msg.attachments ?? [])];
  const marker = '\n\nAttachments:';
  const compactMarker = '\nAttachments:';
  const markerIndex = msg.body.indexOf(marker);
  const compactMarkerIndex = markerIndex === -1 ? msg.body.indexOf(compactMarker) : -1;
  const splitIndex = markerIndex >= 0 ? markerIndex : compactMarkerIndex;
  if (splitIndex === -1) return { body: msg.body, attachments };

  const legacyAttachmentText = msg.body.slice(splitIndex).replace(/^\n+Attachments:\s*/i, '');
  for (const line of legacyAttachmentText.split('\n')) {
    const filename = line.replace(/^\s*-\s*/, '').trim();
    if (filename) attachments.push({ id: `legacy-${msg.id}-${attachments.length}`, filename });
  }
  return { body: msg.body.slice(0, splitIndex).trim(), attachments };
}

function stripInternalLikeableMarkers(value: string): string {
  if (!value.includes('[[LIKEABLE')) return value;
  let out = '';
  let cursor = 0;
  while (cursor < value.length) {
    const start = value.indexOf(LIKEABLE_NOTIFICATION_START, cursor);
    if (start === -1) {
      const rest = value.slice(cursor).replace(/\[\[LIKEABLE_[^\]]*]]/g, '');
      out += rest;
      break;
    }
    out += value.slice(cursor, start);
    const contentStart = start + LIKEABLE_NOTIFICATION_START.length;
    const end = value.indexOf(LIKEABLE_NOTIFICATION_END, contentStart);
    cursor = end === -1 ? value.length : end + LIKEABLE_NOTIFICATION_END.length;
  }
  return out.replace(/\n{3,}/g, '\n\n').trim();
}

function notificationFeedRows(feed: Feed): NotificationFeedRow[] {
  const userTimes = (feed.localMessages ?? [])
    .filter((msg) => msg.role === 'user')
    .map((msg) => Date.parse(msg.createdAt))
    .filter((value) => !Number.isNaN(value))
    .sort((a, b) => a - b);
  const latestUserTime = userTimes.length > 0 ? userTimes[userTimes.length - 1] : null;
  const latestTurnKey = userTimes.length > 0 ? `turn-${userTimes[userTimes.length - 1]}` : '';
  const assistantRows: NotificationFeedRow[] = [];
  const assistantCounters = new Map<string, number>();
  const assistantSources = (feed.messages ?? [])
    .map((msg, sourceIndex) => {
      const time = agentMessageTime(msg);
      return {
        sourceIndex,
        timeValue: Date.parse(time ?? ''),
        time,
        role: msg.role,
        body: msg.body ?? msg.content ?? ''
      };
    })
    .filter((source) => source.role === 'assistant')
    .sort((a, b) => {
      const left = Number.isNaN(a.timeValue) ? Number.MAX_SAFE_INTEGER : a.timeValue;
      const right = Number.isNaN(b.timeValue) ? Number.MAX_SAFE_INTEGER : b.timeValue;
      if (left !== right) return left - right;
      return a.sourceIndex - b.sourceIndex;
    });
  for (const source of assistantSources) {
    const turnKey = turnKeyForTime(userTimes, Number.isNaN(source.timeValue) ? null : source.timeValue) ?? `assistant-${source.sourceIndex}`;
    for (const segment of parseLikeableNotificationSegments(source.body)) {
      const segmentIndex = assistantCounters.get(turnKey) ?? 0;
      assistantCounters.set(turnKey, segmentIndex + 1);
      assistantRows.push({
        kind: 'notification',
        id: `${turnKey}-notification-${segmentIndex}`,
        body: segment.body,
        time: source.time,
        active: false,
        fallback: segment.fallback
      });
    }
  }

  const activityRows: NotificationFeedRow[] = [];
  for (const [index, activity] of (feed.activity ?? []).entries()) {
    const activityTime = agentActivityTime(activity);
    const sourceID = activity.id ?? activityTime ?? `activity-${index}`;
    const body = activity.message ?? '';
    for (const [segmentIndex, segment] of parseLikeableNotificationSegments(body).entries()) {
      activityRows.push({
        kind: 'notification',
        id: `activity-${sourceID}-notification-${segmentIndex}`,
        body: segment.body,
        time: activityTime,
        active: false,
        fallback: segment.fallback
      });
    }
  }

  const rows = dedupeNotifications([...assistantRows, ...activityRows]);
  if (feed.live?.streamText) {
    const liveIdle = feedLiveIdle(feed);
    const liveTurnKey = latestTurnKey || 'live';
    const liveTime = liveNotificationTime(feed.live.startedAt, latestUserTime);
    const liveSegments = parseLikeableNotificationSegments(feed.live.streamText);
    const lastLiveIndex = liveSegments.length - 1;
    for (const [segmentIndex, segment] of liveSegments.entries()) {
      const id = `${liveTurnKey}-notification-${segmentIndex}`;
      if (!segment.streaming && durableNotificationCovers(rows, segment.body, liveTime, id)) continue;
      const isLast = segmentIndex === lastLiveIndex;
      rows.push({
        kind: 'notification',
        id,
        body: segment.body,
        time: liveTime,
        active: isLast && !liveIdle && Boolean(feed.live.isProcessing || segment.streaming),
        fallback: segment.fallback
      });
    }
  }
  return rows;
}

function dedupeNotifications(rows: NotificationFeedRow[]): NotificationFeedRow[] {
  const seen = new Set<string>();
  const out: NotificationFeedRow[] = [];
  for (const row of rows) {
    const body = normalizeBody(row.body);
    if (!body) continue;
    const time = Date.parse(row.time ?? '');
    const timeBucket = Number.isNaN(time) ? '' : String(Math.floor(time / 2000));
    const scope = timeBucket || notificationSequenceKey(row.id) || row.id;
    const key = `${body}:${scope}`;
    if (seen.has(key)) continue;
    seen.add(key);
    out.push(row);
  }
  return out;
}

function durableNotificationCovers(rows: NotificationFeedRow[], body: string, time: string | undefined, notificationID: string): boolean {
  const normalized = normalizeBody(body);
  if (!normalized) return false;
  const targetTime = Date.parse(time ?? '');
  return rows.some((row) => {
    if (row.active) return false;
    const rowTime = Date.parse(row.time ?? '');
    if (!Number.isNaN(targetTime)) {
      if (!Number.isNaN(rowTime) && Math.abs(rowTime - targetTime) > 60_000) return false;
      if (Number.isNaN(rowTime) && notificationSequenceKey(row.id) !== notificationSequenceKey(notificationID)) return false;
    }
    const candidate = normalizeBody(row.body);
    return candidate === normalized || candidate.startsWith(normalized) || normalized.startsWith(candidate);
  });
}

function liveNotificationTime(startedAt: string | undefined, latestUserTime: number | null): string | undefined {
  const startedTime = Date.parse(startedAt ?? '');
  if (latestUserTime == null) return startedAt;
  if (Number.isNaN(startedTime) || startedTime <= latestUserTime) {
    return new Date(latestUserTime + 1).toISOString();
  }
  return startedAt;
}

function turnKeyForTime(userTimes: number[], sourceTime: number | null): string | null {
  if (userTimes.length === 0) return null;
  if (sourceTime == null) return `turn-${userTimes[userTimes.length - 1]}`;
  let selected = userTimes[0];
  for (const time of userTimes) {
    if (time <= sourceTime + 5000) selected = time;
    if (time > sourceTime + 5000) break;
  }
  return `turn-${selected}`;
}

function agentMessageTime(msg: AgentMessage): string | undefined {
  return msg.created_at ?? msg.createdAt;
}

function agentActivityTime(activity: AgentActivity): string | undefined {
  return activity.occurred_at ?? activity.occurredAt;
}

function notificationSequenceKey(notificationID: string): string {
  const marker = '-notification-';
  const index = notificationID.indexOf(marker);
  return index === -1 ? '' : notificationID.slice(0, index);
}

function compareFeedRows(a: FeedRow, b: FeedRow): number {
  const left = Date.parse(a.time ?? '');
  const right = Date.parse(b.time ?? '');
  if (!Number.isNaN(left) && !Number.isNaN(right) && left !== right) return left - right;
  if (a.kind !== b.kind) return a.kind === 'user' ? -1 : 1;
  return a.id.localeCompare(b.id);
}

function parseLikeableNotificationSegments(value: string): Array<{ body: string; streaming: boolean; fallback?: boolean }> {
  const segments: Array<{ body: string; streaming: boolean; fallback?: boolean }> = [];
  let cursor = 0;
  while (cursor < value.length) {
    const start = value.indexOf(LIKEABLE_NOTIFICATION_START, cursor);
    if (start === -1) {
      if (value.indexOf('[[LIKEABLE', cursor) !== -1) {
        segments.push({ body: 'Receiving update', streaming: true, fallback: true });
      }
      break;
    }
    const contentStart = start + LIKEABLE_NOTIFICATION_START.length;
    const end = value.indexOf(LIKEABLE_NOTIFICATION_END, contentStart);
    if (end === -1) {
      const body = value.slice(contentStart).trim();
      segments.push({ body: body || 'Receiving update', streaming: true, fallback: body.length === 0 });
      break;
    }
    const body = value.slice(contentStart, end).trim();
    if (body) segments.push({ body, streaming: false });
    cursor = end + LIKEABLE_NOTIFICATION_END.length;
  }
  return segments;
}

export function feedAwaitingAgent(feed: Feed | null): boolean {
  if (!feed) return false;
  if (feed.live?.isProcessing) return true;
  const latestUser = feedLatestUserTimestamp(feed);
  if (latestUser == null) return false;
  if (feedLiveIdle(feed)) return false;
  if (feedHasAssistantAfterLatestUser(feed)) return false;
  if (feedProjectUpdatedAfterLatestUser(feed)) return false;

  const latestActivity = latestTimestamp((feed.activity ?? []).map(agentActivityTime));
  if (latestActivity != null && latestActivity >= latestUser - TURN_TIME_SLOP_MS) {
    return Date.now() - latestActivity < RECENT_ACTIVITY_WORKING_MS;
  }

  return Date.now() - latestUser < RECENT_UNANSWERED_TURN_MS;
}

export function agentResponseDelayStatus(feed: Feed | null, now = Date.now()): AgentResponseDelayStatus | null {
  if (!feed) return null;
  if (feed.live?.isProcessing) return null;
  const latestUser = feedLatestUserTimestamp(feed);
  if (latestUser == null) return null;
  if (feedHasAssistantAfterLatestUser(feed)) return null;
  if (feedProjectUpdatedAfterLatestUser(feed)) return null;

  const age = now - latestUser;
  if (age < AGENT_WAITING_HINT_MS || age > AGENT_STALE_WARNING_MAX_MS) return null;

  const latestActivity = latestTimestamp((feed.activity ?? []).map(agentActivityTime));
  if (latestActivity != null && latestActivity >= latestUser - TURN_TIME_SLOP_MS && now - latestActivity < RECENT_ACTIVITY_WORKING_MS) {
    return null;
  }

  if (age < AGENT_STALLED_HINT_MS) return { phase: 'waiting', active: true };
  return { phase: 'stalled', active: false, tone: 'warning' };
}

export function feedLiveIdle(feed: Feed | null): boolean {
  if (!feed?.live) return false;
  return feed.live.isProcessing === false && (feed.live.queuedTurns ?? 0) <= 0;
}

function feedLatestUserTimestamp(feed: Feed | null): number | null {
  if (!feed) return null;
  return latestTimestamp((feed.localMessages ?? []).filter((msg) => msg.role === 'user').map((msg) => msg.createdAt));
}

export function feedHasAssistantAfterLatestUser(feed: Feed | null): boolean {
  const latestUser = feedLatestUserTimestamp(feed);
  if (!feed || latestUser == null) return false;
  const latestAssistant = latestTimestamp([
    ...(feed.localMessages ?? []).filter((msg) => msg.role !== 'user').map((msg) => msg.createdAt),
    ...(feed.messages ?? []).filter((msg) => msg.role === 'assistant').map(agentMessageTime)
  ]);
  return latestAssistant != null && latestAssistant >= latestUser - TURN_TIME_SLOP_MS;
}

export function feedProjectUpdatedAfterLatestUser(feed: Feed | null): boolean {
  const latestUser = feedLatestUserTimestamp(feed);
  const projectUpdated = feedProjectUpdatedTimestamp(feed);
  return latestUser != null && projectUpdated != null && projectUpdated >= latestUser + PROJECT_UPDATE_FALLBACK_GRACE_MS;
}

function feedProjectUpdatedTimestamp(feed: Feed | null): number | null {
  if (!feed?.project?.updatedAt) return null;
  const timestamp = Date.parse(feed.project.updatedAt);
  return Number.isNaN(timestamp) ? null : timestamp;
}

function projectUpdateNotificationRow(feed: Feed, notificationRows: NotificationFeedRow[]): NotificationFeedRow | null {
  const latestUser = feedLatestUserTimestamp(feed);
  const projectUpdated = feedProjectUpdatedTimestamp(feed);
  if (latestUser == null || projectUpdated == null || projectUpdated < latestUser + PROJECT_UPDATE_FALLBACK_GRACE_MS) return null;
  const hasRecentNotification = notificationRows.some((row) => {
    if (row.active) return true;
    const rowTime = Date.parse(row.time ?? '');
    return !Number.isNaN(rowTime) && rowTime >= latestUser - TURN_TIME_SLOP_MS;
  });
  if (hasRecentNotification) return null;
  return {
    kind: 'notification',
    id: `project-${feed.project.id}-updated-after-${latestUser}`,
    body: '',
    time: feed.project.updatedAt,
    active: false,
    fallback: true
  };
}

function latestTimestamp(values: Array<string | undefined>): number | null {
  let latest: number | null = null;
  for (const value of values) {
    const timestamp = Date.parse(value ?? '');
    if (Number.isNaN(timestamp)) continue;
    latest = latest == null ? timestamp : Math.max(latest, timestamp);
  }
  return latest;
}

function normalizeBody(body: string): string {
  return body.trim().replace(/\s+/g, ' ');
}

function applyNotificationTimings(rows: FeedRow[], timings?: Record<string, NotificationTiming>, now = Date.now()): FeedRow[] {
  if (!timings) return rows;
  return rows.map((row) => {
    if (row.kind !== 'notification') return row;
    const timing = timings[row.id];
    const elapsedMs = notificationElapsedMs(row, timing, now);
    if (elapsedMs == null) return row;
    return { ...row, elapsedMs, elapsedStartedAt: row.active ? timing?.startedAt : undefined };
  });
}

function notificationElapsedMs(row: NotificationFeedRow, timing: NotificationTiming | undefined, now: number): number | null {
  if (!timing) return null;
  if (typeof timing.elapsedMs === 'number' && Number.isFinite(timing.elapsedMs) && timing.elapsedMs > 0) {
    return timing.elapsedMs;
  }
  if (!row.active) return null;
  const startedAt = Date.parse(timing.startedAt ?? '');
  if (Number.isNaN(startedAt)) return null;
  return Math.max(0, now - startedAt);
}
