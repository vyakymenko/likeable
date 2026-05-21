import { useEffect, useMemo, useRef, useState } from 'react';
import { Loader2, Plus, RefreshCw, Send, Trash2 } from 'lucide-react';
import { cleanPoolRows, makePoolRow, parsePoolRows } from './admin_pool';
import { api } from './api';
import { AppDialog, Metric } from './builder_components';
import { ADMIN_CONFIG_SECTIONS } from './config';
import type { AdminConfigEntry, AdminConfigResponse, AdminProjectSummary, AdminRecoveryResponse, AdminUserDetail, AdminUserSummary, AdminUsersResponse, AgentAssignmentSummary, AgentPoolOption, AgentPoolStat, AppDialogConfig, PoolRow } from './domain';
import { formatBillingDuration, formatMessageTime, formatShortDate, userInitials } from './format';
import { resetCountdownLabels, statusLabel, TranslationKey, useDocumentTitle, useI18n } from './i18n';

function AdminCustomersPanel() {
  const { locale, t } = useI18n();
  const [filters, setFilters] = useState({ q: '', status: '', github: '', billing: '', sort: 'created_desc', perPage: '25' });
  const [page, setPage] = useState(1);
  const [users, setUsers] = useState<AdminUserSummary[]>([]);
  const [total, setTotal] = useState(0);
  const [selectedUserID, setSelectedUserID] = useState('');
  const [detail, setDetail] = useState<AdminUserDetail | null>(null);
  const [agentPool, setAgentPool] = useState<AgentPoolOption[]>([]);
  const [loading, setLoading] = useState(false);
  const [actionError, setActionError] = useState('');
  const [reassigningProjectID, setReassigningProjectID] = useState('');
  const [accessNote, setAccessNote] = useState('');
  const [noticeBody, setNoticeBody] = useState('');
  const [noticeSeverity, setNoticeSeverity] = useState('warning');
  const [dialog, setDialog] = useState<AppDialogConfig | null>(null);
  const noticeListRef = useRef<HTMLDivElement | null>(null);

  const query = () => {
    const params = new URLSearchParams();
    params.set('page', String(page));
    params.set('per_page', filters.perPage);
    for (const key of ['q', 'status', 'github', 'billing', 'sort'] as const) {
      if (filters[key]) params.set(key, filters[key]);
    }
    return params.toString();
  };
  const loadUsers = async () => {
    setLoading(true);
    setActionError('');
    try {
      const response = await api<AdminUsersResponse>(`/api/admin/users?${query()}`);
      setUsers(response.users);
      setAgentPool((current) => response.agentPool ?? current);
      setTotal(response.pagination.total);
      if (selectedUserID && !response.users.some((summary) => summary.user.id === selectedUserID)) {
        setSelectedUserID('');
        setDetail(null);
      }
    } catch (err) {
      setActionError(err instanceof Error ? err.message : t('admin.loadUsersFailed'));
    } finally {
      setLoading(false);
    }
  };
  const loadDetail = async (userID: string) => {
    setActionError('');
    const response = await api<AdminUserDetail>(`/api/admin/users/${userID}`);
    setSelectedUserID(userID);
    setDetail(response);
    setAgentPool((current) => response.agentPool ?? current);
    setAccessNote(response.summary.user.accessNote ?? '');
  };
  useEffect(() => {
    const timer = setTimeout(() => { void loadUsers(); }, 220);
    return () => clearTimeout(timer);
  }, [filters, page]);
  const selectedSummary = detail?.summary;
  const orderedNotices = useMemo(() => [...(detail?.notices ?? [])].sort((left, right) => {
    const leftTime = Date.parse(left.createdAt);
    const rightTime = Date.parse(right.createdAt);
    if (Number.isNaN(leftTime) || Number.isNaN(rightTime)) return 0;
    return leftTime - rightTime;
  }), [detail?.notices]);
  const totalPages = Math.max(1, Math.ceil(total / Number(filters.perPage || 25)));
  const durationLabels = resetCountdownLabels(t);
  const workDuration = (ms?: number) => formatBillingDuration(ms ?? 0, durationLabels);
  const paidHourStatus = (summary: AdminUserSummary) => {
    if (summary.paidHourBalanceMs > 0) return `${workDuration(summary.paidHourBalanceMs)} ${t('profile.hours')}`;
    if (summary.paidHourBalanceMs < 0) return `${workDuration(summary.paidHourBalanceMs)} ${t('common.debt')}`;
    return summary.paidTotalCents > 0 ? t('common.paid') : t('common.unpaid');
  };
  const restrictedUsers = users.filter((summary) => summary.user.accessStatus === 'restricted').length;
  const githubMissingUsers = users.filter((summary) => !summary.githubConnected).length;
  const paidUsers = users.filter((summary) => summary.paidTotalCents > 0 || summary.paidHourBalanceMs > 0).length;

  useEffect(() => {
    const container = noticeListRef.current;
    if (!container) return;
    container.scrollTop = container.scrollHeight;
  }, [selectedUserID, orderedNotices.length]);

  const applyAccess = async (status: 'active' | 'restricted') => {
    if (!selectedUserID) return;
    setActionError('');
    try {
      await api(`/api/admin/users/${selectedUserID}`, {
        method: 'PATCH',
        body: JSON.stringify({ accessStatus: status, accessNote })
      });
      await loadDetail(selectedUserID);
      await loadUsers();
    } catch (err) {
      setActionError(err instanceof Error ? err.message : t('admin.accessUpdateFailed'));
    }
  };
  const patchAccess = async (status: 'active' | 'restricted') => {
    if (!selectedUserID) return;
    if (status === 'restricted') {
      setDialog({
        title: t('admin.restrictDialog.title'),
        body: t('admin.restrictDialog.body'),
        tone: 'danger',
        confirmLabel: t('admin.restrictAccess'),
        onConfirm: () => applyAccess('restricted')
      });
      return;
    }
    await applyAccess(status);
  };
  const sendNotice = async () => {
    if (!selectedUserID || !noticeBody.trim()) return;
    setActionError('');
    try {
      await api(`/api/admin/users/${selectedUserID}/notices`, {
        method: 'POST',
        body: JSON.stringify({ severity: noticeSeverity, body: noticeBody.trim() })
      });
      setNoticeBody('');
      await loadDetail(selectedUserID);
      await loadUsers();
    } catch (err) {
      setActionError(err instanceof Error ? err.message : t('admin.noticeFailed'));
    }
  };
  const handleNoticeKeyDown = (event: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key !== 'Enter' || event.shiftKey || event.nativeEvent.isComposing) return;
    event.preventDefault();
    if (noticeBody.trim()) void sendNotice();
  };
  const applyUnsendNotice = async (noticeID: string) => {
    if (!selectedUserID) return;
    setActionError('');
    try {
      await api(`/api/admin/users/${selectedUserID}/notices/${noticeID}`, { method: 'DELETE' });
      await loadDetail(selectedUserID);
      await loadUsers();
    } catch (err) {
      setActionError(err instanceof Error ? err.message : t('admin.unsendFailed'));
    }
  };
  const unsendNotice = async (noticeID: string) => {
    if (!selectedUserID) return;
    setDialog({
      title: t('admin.unsendDialog.title'),
      body: t('admin.unsendDialog.body'),
      tone: 'warning',
      confirmLabel: t('admin.unsend'),
      onConfirm: () => applyUnsendNotice(noticeID)
    });
  };
  const applyDeleteProject = async (projectID: string) => {
    if (!selectedUserID) return;
    setActionError('');
    try {
      await api(`/api/admin/users/${selectedUserID}/projects/${projectID}`, { method: 'DELETE' });
      await loadDetail(selectedUserID);
      await loadUsers();
    } catch (err) {
      setActionError(err instanceof Error ? err.message : t('admin.projectDeleteFailed'));
    }
  };
  const deleteProject = async (projectID: string) => {
    if (!selectedUserID) return;
    setDialog({
      title: t('admin.deleteProjectDialog.title'),
      body: t('admin.deleteProjectDialog.body'),
      tone: 'danger',
      confirmLabel: t('common.delete'),
      onConfirm: () => applyDeleteProject(projectID)
    });
  };
  const reassignProject = async (projectID: string, assignmentValue: string) => {
    if (!selectedUserID) return;
    const next = parsePairValue(assignmentValue);
    if (!next.agentId || !next.serverId) return;
    setActionError('');
    setReassigningProjectID(projectID);
    try {
      const response = await api<{ detail: AdminUserDetail; warning?: string }>(`/api/admin/users/${selectedUserID}/projects/${projectID}/assignment`, {
        method: 'PATCH',
        body: JSON.stringify({ agent_id: next.agentId, server_id: next.serverId })
      });
      setDetail(response.detail);
      setAgentPool((current) => response.detail.agentPool ?? current);
      if (response.warning) setActionError(response.warning);
      await loadUsers();
    } catch (err) {
      setActionError(err instanceof Error ? err.message : t('admin.assignment.failed'));
    } finally {
      setReassigningProjectID('');
    }
  };

  return (
    <section className="adminCard customersCard">
      {dialog && <AppDialog dialog={dialog} onClose={() => setDialog(null)} />}
      <div className="adminCardHeader">
        <h3>{t('admin.customers.title')}</h3>
        <p>{t('admin.customers.body')}</p>
      </div>
      <div className="adminOpsGrid">
        <div>
          <span>{t('admin.ops.customers')}</span>
          <strong>{total}</strong>
          <em>{t('admin.ops.pageLoaded', { count: users.length })}</em>
        </div>
        <div>
          <span>{t('admin.ops.restricted')}</span>
          <strong>{restrictedUsers}</strong>
          <em>{t('admin.ops.activeVisible', { count: Math.max(0, users.length - restrictedUsers) })}</em>
        </div>
        <div>
          <span>{t('admin.ops.githubMissing')}</span>
          <strong>{githubMissingUsers}</strong>
          <em>{t('admin.ops.paidVisible', { count: paidUsers })}</em>
        </div>
      </div>
      <div className="customerFilters">
        <label className="configLabel">
          <span>{t('admin.search')}</span>
          <input value={filters.q} placeholder={t('admin.search.placeholder')} onChange={(event) => { setPage(1); setFilters({ ...filters, q: event.target.value }); }} />
        </label>
        <label className="configLabel">
          <span>{t('admin.access')}</span>
          <select className="adminSelect" value={filters.status} onChange={(event) => { setPage(1); setFilters({ ...filters, status: event.target.value }); }}>
            <option value="">{t('common.all')}</option>
            <option value="active">{t('common.active')}</option>
            <option value="restricted">{t('common.restricted')}</option>
          </select>
        </label>
        <label className="configLabel">
          <span>{t('common.github')}</span>
          <select className="adminSelect" value={filters.github} onChange={(event) => { setPage(1); setFilters({ ...filters, github: event.target.value }); }}>
            <option value="">{t('common.all')}</option>
            <option value="connected">{t('common.connected')}</option>
            <option value="missing">{t('common.missing')}</option>
          </select>
        </label>
        <label className="configLabel">
          <span>{t('admin.billing')}</span>
          <select className="adminSelect" value={filters.billing} onChange={(event) => { setPage(1); setFilters({ ...filters, billing: event.target.value }); }}>
            <option value="">{t('common.all')}</option>
            <option value="paid">{t('common.paid')}</option>
            <option value="unpaid">{t('common.unpaid')}</option>
            <option value="subscribed">{t('admin.subscriptionLegacy')}</option>
          </select>
        </label>
        <label className="configLabel">
          <span>{t('admin.sort')}</span>
          <select className="adminSelect" value={filters.sort} onChange={(event) => setFilters({ ...filters, sort: event.target.value })}>
            <option value="created_desc">{t('admin.sort.newest')}</option>
            <option value="hours_desc">{t('admin.sort.hours')}</option>
            <option value="paid_desc">{t('admin.sort.paid')}</option>
            <option value="projects_desc">{t('admin.sort.projects')}</option>
            <option value="email_asc">{t('admin.sort.email')}</option>
          </select>
        </label>
      </div>
      {actionError && <div className="adminError inlineAdminError">{actionError}</div>}
      <div className="customersLayout">
        <div className="customerList" aria-busy={loading}>
          {users.map((summary) => (
            <button className={`customerRow ${selectedUserID === summary.user.id ? 'selected' : ''} ${summary.user.accessStatus === 'restricted' ? 'restricted' : ''}`} key={summary.user.id} onClick={() => void loadDetail(summary.user.id)}>
              <span className="customerAvatar" aria-hidden="true">{userInitials(summary.user)}</span>
              <span className="customerIdentity">
                <strong>{summary.user.email}</strong>
                <em>{summary.user.name || summary.user.id}</em>
              </span>
              <span className="customerStats">
                <b>{workDuration(summary.windowWorkMs)}/{workDuration(summary.freeHourLimitMs)}</b>
                <small>{workDuration(summary.lifetimeWorkMs)} {t('admin.lifetime')} · {summary.projectCount}/{summary.projectLimit ?? summary.projectCount} {t('common.projects')}</small>
              </span>
              <span className="customerBadges">
                <i>{summary.githubConnected ? t('common.github') : t('admin.noGithub')}</i>
                <i>{paidHourStatus(summary)}</i>
                {summary.user.accessStatus === 'restricted' && <i className="dangerBadge">{t('common.restricted')}</i>}
                {(summary.agentPairs ?? []).slice(0, 2).map((pair) => (
                  <i className="assignmentBadge" key={`${pair.agentId}:${pair.serverId}`}>
                    {formatPairBadge(pair)}
                  </i>
                ))}
              </span>
            </button>
          ))}
          {users.length === 0 && <div className="emptyPool">{loading ? t('admin.loadingUsers') : t('admin.noUsers')}</div>}
          <div className="paginationRow">
            <button className="ghostButton" disabled={page <= 1} onClick={() => setPage((value) => Math.max(1, value - 1))}>{t('admin.previous')}</button>
            <span>{t('admin.pagination', { page, totalPages, total })}</span>
            <button className="ghostButton" disabled={page >= totalPages} onClick={() => setPage((value) => Math.min(totalPages, value + 1))}>{t('admin.next')}</button>
          </div>
        </div>
        <div className="customerDetail">
          {!selectedSummary ? (
            <div className="emptyPool">{t('admin.selectUser')}</div>
          ) : (
            <>
              <div className="customerDetailHeader">
                <div>
                  <span className="eyebrow">{t('admin.customer')}</span>
                  <strong>{selectedSummary.user.email}</strong>
                  <em>{selectedSummary.user.id}</em>
                </div>
                <span className={`accessBadge ${selectedSummary.user.accessStatus === 'restricted' ? 'restricted' : ''}`}>{selectedSummary.user.accessStatus === 'restricted' ? t('common.restricted') : t('common.active')}</span>
              </div>
              <div className="customerMetricGrid">
                <Metric label={t('admin.metric.freeWindowHours')} value={`${workDuration(selectedSummary.windowWorkMs)}/${workDuration(selectedSummary.freeHourLimitMs)}`} />
                <Metric label={t('admin.metric.lifetimeHours')} value={workDuration(selectedSummary.lifetimeWorkMs)} />
                <Metric label={t('admin.metric.paidHours')} value={workDuration(selectedSummary.paidHourBalanceMs)} />
                <Metric label={t('admin.metric.projects')} value={`${selectedSummary.projectCount}/${selectedSummary.projectLimit ?? selectedSummary.projectCount}`} />
                <Metric label={t('admin.metric.paidSlots')} value={selectedSummary.paidProjectSlots ? `${selectedSummary.paidProjectSlots}${selectedSummary.projectSlotsExpire ? ` ${t('common.until')} ${formatShortDate(selectedSummary.projectSlotsExpire, locale)}` : ''}` : '0'} />
                <Metric label={t('admin.metric.github')} value={selectedSummary.githubConnected ? t('common.connected') : t('common.missing')} />
                <Metric label={t('admin.metric.paid')} value={formatMoney(selectedSummary.paidTotalCents, selectedSummary.paidCurrency)} />
              </div>
              <div className="moderationBox">
                <label className="configLabel">
                  <span>{t('admin.accessNote')}</span>
                  <textarea className="adminTextarea compactTextarea" rows={3} value={accessNote} onChange={(event) => setAccessNote(event.target.value)} placeholder={t('admin.accessNote.placeholder')} />
                </label>
                <div className="dialogActions compactActions">
                  <button className="ghostButton" onClick={() => void patchAccess('active')}>{t('admin.restoreAccess')}</button>
                  <button className="dangerButton" onClick={() => void patchAccess('restricted')}>{t('admin.restrictAccess')}</button>
                </div>
              </div>
              <div className="adminMessageConsole">
                <div className="adminCardHeader compactHeader">
                  <h3>{t('admin.messages.title')}</h3>
                  <p>{t('admin.messages.body')}</p>
                </div>
                <div className="adminNoticeList" ref={noticeListRef}>
                  {orderedNotices.map((notice) => (
                    <div className={`adminNoticeBubble ${notice.sender === 'user' ? 'incoming' : 'outgoing'} ${notice.severity} ${notice.unsentAt ? 'unsent' : ''}`} key={notice.id}>
                      <div className="adminNoticeBubbleMeta">
                        <span>{notice.sender === 'user' ? t('common.user') : notice.sender === 'system' ? t('common.system') : t('common.admin')}</span>
                        <time dateTime={notice.createdAt}>{formatMessageTime(notice.createdAt, locale)}</time>
                        {notice.sender !== 'user' && !notice.unsentAt && (
                          <button className="unsendNoticeButton" onClick={() => void unsendNotice(notice.id)}>{t('admin.unsend')}</button>
                        )}
                      </div>
                      <p>{notice.body}</p>
                      {notice.dismissedAt && <em>{t('admin.dismissedByUser')}</em>}
                      {notice.unsentAt && <em>{t('admin.unsent')}</em>}
                    </div>
                  ))}
                  {orderedNotices.length === 0 && <div className="adminNoticeEmpty">{t('admin.noMessages')}</div>}
                </div>
                <div className="adminNoticeComposer">
                  <label className="noticeSeverityControl">
                    <span>{t('admin.severity')}</span>
                    <select className="adminSelect" value={noticeSeverity} onChange={(event) => setNoticeSeverity(event.target.value)}>
                      <option value="info">{t('common.info')}</option>
                      <option value="warning">{t('common.warning')}</option>
                      <option value="danger">{t('common.danger')}</option>
                    </select>
                  </label>
                  <textarea
                    className="adminTextarea compactTextarea"
                    rows={1}
                    value={noticeBody}
                    onChange={(event) => setNoticeBody(event.target.value)}
                    onKeyDown={handleNoticeKeyDown}
                    placeholder={t('admin.notice.placeholder')}
                  />
                  <button className="primaryButton noticeSendButton" disabled={!noticeBody.trim()} onClick={() => void sendNotice()}><Send size={16} /> {t('admin.send')}</button>
                </div>
              </div>
              <div className="adminProjects">
                <div className="adminCardHeader compactHeader">
                  <h3>{t('admin.projects.title')}</h3>
                  <p>{t('admin.projects.body')}</p>
                </div>
                {detail.projects.map((item) => {
                  const options = assignmentOptionsForProject(item, agentPool);
                  const assignment = item.assignment;
                  const assignmentValue = pairValue(assignment?.agentId ?? '', assignment?.serverId ?? '');
                  const canReassign = options.some((option) => option.status === 'active') && item.project.status !== 'archived' && item.project.status !== 'deleting';
                  const previewUrl = item.project.previewUrl;
                  return (
                    <div className="adminProjectRow" key={item.project.id}>
                      <span>
                        <strong>{item.project.title}</strong>
                        <em>{statusLabel(item.project.status, t)} · {workDuration(item.workMs)}</em>
                      </span>
                      <label className="adminProjectAssignment">
                        <span>{t('admin.assignment')}</span>
                        <select
                          className="adminSelect"
                          value={assignmentValue}
                          disabled={!canReassign || reassigningProjectID === item.project.id}
                          onChange={(event) => void reassignProject(item.project.id, event.target.value)}
                        >
                          {assignmentValue === pairValue('', '') && <option value={assignmentValue}>{t('admin.assignment.none')}</option>}
                          {options.map((option) => (
                            <option key={pairValue(option.agentId, option.serverId)} value={pairValue(option.agentId, option.serverId)} disabled={option.status !== 'active'}>
                              {formatPoolOption(option, t)}
                            </option>
                          ))}
                        </select>
                      </label>
                      <div className="adminProjectActions">
                        {previewUrl && <a className="ghostButton" href={previewUrl} target="_blank" rel="noopener noreferrer">{t('common.open')}</a>}
                        <button className="projectDelete" onClick={() => void deleteProject(item.project.id)} aria-label={t('admin.deleteProject.aria', { title: item.project.title })}><Trash2 size={15} /></button>
                      </div>
                    </div>
                  );
                })}
                {detail.projects.length === 0 && <div className="emptyPool">{t('admin.noActiveProjects')}</div>}
              </div>
            </>
          )}
        </div>
      </div>
    </section>
  );
}

function formatMoney(cents: number, currency: string): string {
  if (!cents) return '0';
  const normalized = (currency || 'usd').toUpperCase();
  return `${normalized} ${(cents / 100).toFixed(2)}`;
}

function pairValue(agentId: string, serverId: string): string {
  return JSON.stringify([agentId, serverId]);
}

function parsePairValue(value: string): { agentId: string; serverId: string } {
  try {
    const parsed = JSON.parse(value) as unknown;
    if (Array.isArray(parsed) && typeof parsed[0] === 'string' && typeof parsed[1] === 'string') {
      return { agentId: parsed[0], serverId: parsed[1] };
    }
  } catch {
    return { agentId: '', serverId: '' };
  }
  return { agentId: '', serverId: '' };
}

function formatPairBadge(pair: AgentAssignmentSummary): string {
  const count = pair.projectCount && pair.projectCount > 1 ? ` x${pair.projectCount}` : '';
  return `${pair.agentId}/${pair.serverId}${count}`;
}

function assignmentOptionsForProject(item: AdminProjectSummary, agentPool: AgentPoolOption[]): AgentPoolOption[] {
  const current = item.assignment;
  const out: AgentPoolOption[] = [];
  const pushUnique = (option: AgentPoolOption) => {
    if (!option.agentId || !option.serverId) return;
    if (out.some((candidate) => candidate.agentId === option.agentId && candidate.serverId === option.serverId)) return;
    out.push(option);
  };
  if (current?.agentId || current?.serverId) {
    pushUnique({
      agentId: current.agentId,
      serverId: current.serverId,
      status: current.status ?? 'retired'
    });
  }
  agentPool.filter((option) => option.status === 'active').forEach(pushUnique);
  return out;
}

function formatPoolOption(option: AgentPoolOption, t: (key: TranslationKey) => string): string {
  const label = option.label ? `${option.label} · ` : '';
  const status = poolStatusLabel(option.status, t);
  return `${label}${option.agentId}/${option.serverId} · ${status}`;
}

function poolStatusLabel(status: string | undefined, t: (key: TranslationKey) => string): string {
  switch (status) {
    case 'active':
      return t('admin.pool.status.active');
    case 'draining':
      return t('admin.pool.status.draining');
    case 'retiring':
      return t('admin.pool.status.retiring');
    case 'retired':
      return t('admin.pool.status.retired');
    default:
      return t('status.unknown');
  }
}

function adminConfigLabel(key: string, t: (key: TranslationKey) => string): string {
  const labelKeys: Record<string, TranslationKey> = {
    github_client_id: 'admin.config.github_client_id',
    github_client_secret: 'admin.config.github_client_secret',
    github_username: 'admin.config.github_username',
    github_token: 'admin.config.github_token',
    google_client_id: 'admin.config.google_client_id',
    google_client_secret: 'admin.config.google_client_secret',
    stripe_publishable_key: 'admin.config.stripe_publishable_key',
    stripe_secret_key: 'admin.config.stripe_secret_key',
    stripe_price_id_1_hour: 'admin.config.stripe_price_id_1_hour',
    stripe_price_id_10_hours: 'admin.config.stripe_price_id_10_hours',
    stripe_price_id_100_hours: 'admin.config.stripe_price_id_100_hours',
    stripe_project_quota_price_id: 'admin.config.stripe_project_quota_price_id',
    stripe_webhook_secret: 'admin.config.stripe_webhook_secret',
    free_hours: 'admin.config.free_hours',
    free_hour_window_hours: 'admin.config.free_hour_window_hours',
    project_cap: 'admin.config.project_cap'
  };
  const labelKey = labelKeys[key];
  return labelKey ? t(labelKey) : key;
}

export function Admin() {
  const { locale, t } = useI18n();
  useDocumentTitle(t('page.adminSettings.title'));
  const [config, setConfig] = useState<Record<string, AdminConfigEntry>>({});
  const [draft, setDraft] = useState<Record<string, string>>({});
  const [signupMode, setSignupMode] = useState('forbidden');
  const [allowedEmails, setAllowedEmails] = useState('');
  const [poolRows, setPoolRows] = useState<PoolRow[]>([]);
  const [poolStats, setPoolStats] = useState<AgentPoolStat[]>([]);
  const [retiringPoolRowID, setRetiringPoolRowID] = useState('');
  const [saving, setSaving] = useState(false);
  const [status, setStatus] = useState('');
  const [error, setError] = useState('');
  const [recovery, setRecovery] = useState<AdminRecoveryResponse | null>(null);
  const [recoveryLoading, setRecoveryLoading] = useState(false);
  const [recoveryError, setRecoveryError] = useState('');

  const loadConfig = async () => {
    const response = await api<AdminConfigResponse>('/api/admin/config');
    setConfig(response.config);
    setDraft({});
    setSignupMode(response.config.signup_mode?.value ?? 'forbidden');
    setAllowedEmails(response.config.signup_allowed_emails?.value ?? '');
    setPoolRows(parsePoolRows(response.config.fibe_agent_server_pool?.value ?? '[]'));
    setPoolStats(response.agentPoolStats ?? []);
  };

  const loadRecovery = async () => {
    setRecoveryLoading(true);
    setRecoveryError('');
    try {
      setRecovery(await api<AdminRecoveryResponse>('/api/admin/recovery'));
    } catch (err) {
      setRecoveryError(err instanceof Error ? err.message : t('admin.recovery.loadFailed'));
    } finally {
      setRecoveryLoading(false);
    }
  };

  useEffect(() => {
    void loadConfig();
    void loadRecovery();
  }, []);

  const setPoolRow = (id: string, patch: Partial<PoolRow>) => {
    setPoolRows((rows) => rows.map((row) => row.id === id ? { ...row, ...patch } : row));
  };
  const statForPoolRow = (row: PoolRow) => poolStats.find((stat) => stat.agentId === row.agentId.trim() && stat.serverId === row.serverId.trim());
  const retirePoolRow = async (row: PoolRow) => {
    setRetiringPoolRowID(row.id);
    setStatus('');
    setError('');
    try {
      await api('/api/admin/agent-pool/retire', {
        method: 'POST',
        body: JSON.stringify({ agent_id: row.agentId.trim(), server_id: row.serverId.trim() })
      });
      await loadConfig();
      setStatus(t('admin.pool.retired'));
    } catch (err) {
      setError(err instanceof Error ? err.message : t('admin.saveFailed'));
    } finally {
      setRetiringPoolRowID('');
    }
  };

  const save = async () => {
    setSaving(true);
    setStatus('');
    setError('');
    try {
      const pool = cleanPoolRows(poolRows);
      const incomplete = pool.find((row) => !row.agent_id || !row.server_id);
      if (incomplete) {
        throw new Error(t('admin.poolIncomplete'));
      }
      await api('/api/admin/config', {
        method: 'PUT',
        body: JSON.stringify({
          ...draft,
          signup_mode: signupMode,
          signup_allowed_emails: allowedEmails,
          fibe_agent_server_pool: JSON.stringify(pool)
        })
      });
      await loadConfig();
      setStatus(t('common.saved'));
    } catch (err) {
      setError(err instanceof Error ? err.message : t('admin.saveFailed'));
    } finally {
      setSaving(false);
    }
  };

  const renderConfigFields = (keys: string[]) => {
    const entries = keys.map((key) => [key, config[key] ?? { value: '', secret: false, set: false }] as const);
    if (entries.length === 0) return <div className="emptyPool">{t('admin.noSettings')}</div>;
    return (
      <div className="configGrid">
        {entries.map(([key, meta]) => (
          <label key={key} className="configLabel">
            <span className="configLabelText">
              <strong>{adminConfigLabel(key, t)}</strong>
              {adminConfigLabel(key, t) !== key && <em>{key}</em>}
            </span>
            <input
              type={meta.secret ? 'password' : 'text'}
              placeholder={meta.secret && meta.set ? t('admin.config.set') : meta.value}
              value={draft[key] ?? (meta.secret ? '' : meta.value)}
              onChange={(event) => setDraft({ ...draft, [key]: event.target.value })}
            />
          </label>
        ))}
      </div>
    );
  };

  return (
    <section className="panel adminPanel">
      <div className="panelTitleRow">
        <h2>{t('admin.panel.title')}</h2>
        {status && <span className="adminStatus">{status}</span>}
        {error && <span className="adminError">{error}</span>}
      </div>

      <div className="adminStack">
        <AdminCustomersPanel />

        <section className="adminCard recoveryCard">
          <div className="adminCardHeader withAction">
            <div>
              <h3>{t('admin.recovery.title')}</h3>
              <p>{t('admin.recovery.body')}</p>
            </div>
            <button className="ghostButton" type="button" disabled={recoveryLoading} onClick={() => void loadRecovery()}>
              {recoveryLoading ? <Loader2 className="spinIcon" size={15} /> : <RefreshCw size={15} />}
              {t('admin.recovery.refresh')}
            </button>
          </div>
          {recoveryError && <div className="adminError inlineAdminError">{recoveryError}</div>}
          <div className="customerMetricGrid recoveryMetricGrid">
            <Metric label={t('admin.recovery.pendingAccounts')} value={String(recovery?.pendingAccountDeletionCount ?? 0)} />
            <Metric label={t('admin.recovery.deletingProjects')} value={String(recovery?.deletingProjectCount ?? 0)} />
            <Metric label={t('admin.recovery.sweep')} value={t('admin.recovery.sweepEvery', { seconds: recovery?.sweepIntervalSeconds ?? 0 })} />
            <Metric label={t('admin.recovery.checkedAt')} value={recovery?.checkedAt ? formatMessageTime(recovery.checkedAt, locale) : '—'} />
          </div>
          {recovery && recovery.pendingAccountDeletions.length === 0 && recovery.deletingProjects.length === 0 && <div className="emptyPool">{t('admin.recovery.noIssues')}</div>}
          {recovery && (recovery.pendingAccountDeletions.length > 0 || recovery.deletingProjects.length > 0) && (
            <div className="recoveryList">
              {recovery.pendingAccountDeletions.map((account) => (
                <div className={`recoveryRow ${account.ready ? 'ready' : 'waiting'}`} key={`account-${account.userId}`}>
                  <span>
                    <strong>{account.email}</strong>
                    <em>{account.ready ? t('admin.recovery.accountReady') : t('admin.recovery.accountWaiting', { projects: account.projectCount })}</em>
                  </span>
                  <code>{account.userId}</code>
                </div>
              ))}
              {recovery.deletingProjects.map((project) => (
                <div className={`recoveryRow ${project.cleanupLastError ? 'failed' : 'waiting'}`} key={`project-${project.id}`}>
                  <span>
                    <strong>{project.title}</strong>
                    <em>{project.cleanupLastError || t('admin.recovery.projectWaiting')}</em>
                  </span>
                  <code>{project.id}</code>
                </div>
              ))}
            </div>
          )}
        </section>

        <section className="adminCard">
          <div className="adminCardHeader">
            <h3>{t('admin.accessCard.title')}</h3>
            <p>{t('admin.accessCard.body')}</p>
          </div>
          <label className="configLabel compactConfigLabel">
            <span>{t('admin.signupMode.label')}</span>
            <select className="adminSelect" value={signupMode} onChange={(event) => setSignupMode(event.target.value)}>
              <option value="forbidden">{t('admin.signupMode.forbidden')}</option>
              <option value="allowlist">{t('admin.signupMode.allowlist')}</option>
              <option value="all">{t('admin.signupMode.all')}</option>
            </select>
          </label>
          {signupMode === 'allowlist' && (
            <label className="configLabel">
              <span>{t('admin.signupAllowedEmails.label')}</span>
              <textarea
                className="adminTextarea"
                rows={7}
                spellCheck={false}
                placeholder={t('admin.signupAllowedEmails.placeholder')}
                value={allowedEmails}
                onChange={(event) => setAllowedEmails(event.target.value)}
              />
            </label>
          )}
        </section>

        <section className="adminCard integrationCard">
          <div className="adminCardHeader">
            <h3>{t('admin.fibe.title')}</h3>
            <p>{t('admin.fibe.body')}</p>
          </div>
          {renderConfigFields(['fibe_base_url', 'fibe_api_key'])}
          <div className="adminCardHeader withAction">
            <div>
              <h3>{t('admin.pool.title')}</h3>
              <p>{t('admin.pool.body')}</p>
            </div>
            <button className="ghostButton" type="button" onClick={() => setPoolRows((rows) => [...rows, makePoolRow()])}>
              <Plus size={17} /> {t('admin.pool.add')}
            </button>
          </div>
          <div className="poolRows">
            {poolRows.length === 0 && <div className="emptyPool">{t('admin.pool.empty')}</div>}
            {poolRows.map((row, index) => (
              <div className={`poolRow ${row.status}`} key={row.id}>
                <label>
                  <span>{t('admin.pool.label')}</span>
                  <input value={row.label} placeholder={t('admin.pool.pair', { number: index + 1 })} onChange={(event) => setPoolRow(row.id, { label: event.target.value })} />
                </label>
                <label>
                  <span>{t('admin.pool.agentId')}</span>
                  <input value={row.agentId} placeholder={t('admin.pool.agentPlaceholder')} onChange={(event) => setPoolRow(row.id, { agentId: event.target.value })} />
                </label>
                <label>
                  <span>{t('admin.pool.serverId')}</span>
                  <input value={row.serverId} placeholder={t('admin.pool.serverPlaceholder')} onChange={(event) => setPoolRow(row.id, { serverId: event.target.value })} />
                </label>
                <label>
                  <span>{t('admin.pool.status')}</span>
                  <select className="adminSelect" value={row.status} onChange={(event) => setPoolRow(row.id, { status: event.target.value as PoolRow['status'] })}>
                    <option value="active">{t('admin.pool.status.active')}</option>
                    <option value="draining">{t('admin.pool.status.draining')}</option>
                    <option value="retiring" disabled>{t('admin.pool.status.retiring')}</option>
                    <option value="retired" disabled>{t('admin.pool.status.retired')}</option>
                  </select>
                </label>
                <div className="poolStatLine">
                  {(() => {
                    const stat = statForPoolRow(row);
                    return stat
                      ? t('admin.pool.stats', { projects: stat.projectCount, archived: stat.archivedCount, archives: stat.readyArchiveCount })
                      : t('admin.pool.stats.empty');
                  })()}
                </div>
                <button className="ghostButton poolRetireButton" type="button" disabled={saving || retiringPoolRowID === row.id || !row.agentId.trim() || !row.serverId.trim() || row.status === 'retired'} onClick={() => void retirePoolRow(row)}>
                  {retiringPoolRowID === row.id ? <Loader2 className="spinIcon" size={15} /> : null}
                  {t('admin.pool.retire')}
                </button>
                <button className="smallIconButton" type="button" disabled={row.status === 'retiring' || row.status === 'retired'} aria-label={t('admin.pool.remove')} onClick={() => setPoolRows((rows) => rows.filter((candidate) => candidate.id !== row.id))}>
                  <Trash2 size={17} />
                </button>
              </div>
            ))}
          </div>
        </section>

        {ADMIN_CONFIG_SECTIONS.slice(1).map((section) => (
          <section className="adminCard integrationCard" key={section.titleKey}>
            <div className="adminCardHeader">
              <h3>{t(section.titleKey as TranslationKey)}</h3>
              <p>{t(section.bodyKey as TranslationKey)} {t('admin.config.secretHelp')}</p>
            </div>
            {renderConfigFields(section.keys)}
          </section>
        ))}
      </div>

      <button className="primaryButton adminSave" disabled={saving} onClick={save}>
        {saving ? <><Loader2 size={17} className="spin" /> {t('common.saving')}</> : t('common.save')}
      </button>
    </section>
  );
}
