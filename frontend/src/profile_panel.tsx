import { useEffect, useMemo, useRef, useState } from 'react';
import { BookOpen, Check, CircleAlert, ExternalLink, FolderOpen, GitBranch, Loader2, LogOut, Send, Trash2, Wallet, X } from 'lucide-react';
import { api } from './api';
import { DeleteAllAccountDialog } from './builder_components';
import type { Me, ProjectArchive, UserNotice } from './domain';
import { formatBillingDuration, formatMessageTime, formatResetCountdown, formatShortDate, userInitials } from './format';
import { resetCountdownLabels, statusLabel, useI18n } from './i18n';

export function ProfilePanel({ me, onClose, onOpenTutorial }: { me: Me; onClose: () => void; onOpenTutorial: () => void }) {
  const { locale, t } = useI18n();
  const [messages, setMessages] = useState<UserNotice[]>([]);
  const [archives, setArchives] = useState<ProjectArchive[]>([]);
  const [supportBody, setSupportBody] = useState('');
  const [busyPack, setBusyPack] = useState<number | null>(null);
  const [sendingSupport, setSendingSupport] = useState(false);
  const [profileError, setProfileError] = useState('');
  const [deleteAllOpen, setDeleteAllOpen] = useState(false);
  const [deletingAll, setDeletingAll] = useState(false);
  const profileMessagesRef = useRef<HTMLDivElement | null>(null);
  const orderedMessages = useMemo(() => messages.filter((message) => !isTransientProfileMessage(message, Date.now())).sort((left, right) => {
    const leftTime = Date.parse(left.createdAt);
    const rightTime = Date.parse(right.createdAt);
    if (Number.isNaN(leftTime) || Number.isNaN(rightTime)) return 0;
    return leftTime - rightTime;
  }), [messages]);
  const loadMessages = async () => {
    try {
      const res = await api<{ messages: UserNotice[] }>('/api/messages?limit=80');
      setMessages(res.messages ?? []);
    } catch (err) {
      setProfileError(err instanceof Error ? err.message : t('profile.loadMessagesFailed'));
    }
  };
  const loadArchives = async () => {
    try {
      const res = await api<{ archives: ProjectArchive[] }>('/api/profile/archives');
      setArchives(res.archives ?? []);
    } catch (err) {
      setProfileError(err instanceof Error ? err.message : t('profile.loadArchivesFailed'));
    }
  };
  useEffect(() => {
    void loadMessages();
    void loadArchives();
  }, []);
  useEffect(() => {
    const container = profileMessagesRef.current;
    if (!container) return;
    container.scrollTop = container.scrollHeight;
  }, [orderedMessages.length]);
  const checkout = async (pack: number) => {
    setBusyPack(pack);
    setProfileError('');
    try {
      const res = await api<{ url: string }>('/api/billing/checkout', {
        method: 'POST',
        body: JSON.stringify({ product: 'hour_pack', pack })
      });
      location.href = res.url;
    } catch (err) {
      setProfileError(err instanceof Error ? err.message : t('profile.checkoutFailed'));
      setBusyPack(null);
    }
  };
  const checkoutProjectSlot = async () => {
    setBusyPack(-1);
    setProfileError('');
    try {
      const res = await api<{ url: string }>('/api/billing/checkout', {
        method: 'POST',
        body: JSON.stringify({ product: 'project_quota', slots: 1 })
      });
      location.href = res.url;
    } catch (err) {
      setProfileError(err instanceof Error ? err.message : t('profile.checkoutFailed'));
      setBusyPack(null);
    }
  };
  const sendSupportMessage = async () => {
    if (!supportBody.trim()) return;
    setSendingSupport(true);
    setProfileError('');
    try {
      await api('/api/messages', {
        method: 'POST',
        body: JSON.stringify({ body: supportBody.trim() })
      });
      setSupportBody('');
      await loadMessages();
    } catch (err) {
      setProfileError(err instanceof Error ? err.message : t('profile.messageFailed'));
    } finally {
      setSendingSupport(false);
    }
  };
  const handleSupportKeyDown = (event: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key !== 'Enter' || event.shiftKey || event.nativeEvent.isComposing) return;
    event.preventDefault();
    if (supportBody.trim() && !sendingSupport) void sendSupportMessage();
  };
  const deleteAll = async (email: string) => {
    setDeletingAll(true);
    setProfileError('');
    try {
      await api('/api/profile/delete-all', {
        method: 'POST',
        body: JSON.stringify({ email })
      });
      location.href = '/';
    } catch (err) {
      setProfileError(err instanceof Error ? err.message : t('profile.deleteAllFailed'));
      setDeletingAll(false);
    }
  };
  const displayName = me.user?.name || me.user?.email || t('profile.signedIn');
  const quota = me.hourQuota;
  const projectQuota = me.projectQuota;
  const availableHourPacks = me.billingProducts?.hourPacks ?? [];
  const projectQuotaPurchasable = Boolean(me.billingProducts?.projectQuota);
  const quotaResetLabel = quota ? formatResetCountdown(quota.resetsAt, Date.now(), resetCountdownLabels(t)) : t('duration.fiveHours');
  const quotaWindowHours = quota?.windowHours ?? 5;
  const remainingHours = formatBillingDuration(quota?.remainingMs ?? 0, resetCountdownLabels(t));
  const limitHours = formatBillingDuration(quota?.limitMs ?? 0, resetCountdownLabels(t));
  const paidHours = formatBillingDuration(quota?.paidRemainingMs ?? 0, resetCountdownLabels(t));
  const lifetimeHours = formatBillingDuration(quota?.lifetimeUsedMs ?? 0, resetCountdownLabels(t));
  const quotaUsedPercent = quota?.limitMs ? Math.min(100, Math.max(0, ((quota.usedMs ?? 0) / quota.limitMs) * 100)) : 0;
  const projectSlotPercent = projectQuota?.limit ? Math.min(100, Math.max(0, (projectQuota.used / projectQuota.limit) * 100)) : 0;
  const projectSlotsFull = Boolean(projectQuota && projectQuota.used >= projectQuota.limit);
  return (
    <section className="inlinePanel profileInline">
      <div className="inlinePanelHeader">
        <div>
          <span className="eyebrow">{t('profile.title')}</span>
          <strong>{me.user?.email}</strong>
        </div>
        <button className="projectDelete" onClick={onClose} aria-label={t('profile.close')}><X size={15} /></button>
      </div>
      {profileError && <div className="adminError inlineAdminError">{profileError}</div>}
      <div className="profileGrid">
        <div className="profileCard profileIdentityCard">
          <div className="profileAvatar">{userInitials(me.user)}</div>
          <div>
            <span className="profileLabel">{t('profile.signedInAs')}</span>
            <strong>{displayName}</strong>
            <em>{me.user?.email}</em>
          </div>
          {me.isAdmin && <span className="profileBadge">{t('common.admin')}</span>}
        </div>
        <div className="profileCard profileActionCard">
          <div>
            <span className="profileLabel">{t('common.github')}</span>
            <strong>{t('profile.githubExport')}</strong>
          </div>
          {me.githubConnected && !me.githubNeedsReconnect
            ? <span className="profileConnected"><Check size={15} /> {t('profile.connected')}</span>
            : <a className="ghostButton" href="/api/profile/github/start"><GitBranch size={18} /> {me.githubNeedsReconnect ? t('profile.reconnect') : t('profile.connect')}</a>}
        </div>
        <div className="profileCard profileActionCard">
          <div>
            <span className="profileLabel">{t('profile.tutorial')}</span>
            <strong>{t('profile.tutorialTitle')}</strong>
            <em>{t('profile.tutorialBody')}</em>
          </div>
          <button className="ghostButton" onClick={onOpenTutorial}>
            <BookOpen size={16} /> {t('profile.tutorialOpen')}
          </button>
        </div>
        <div className="profileCard profileActionCard profileHoursCard">
          <div>
            <span className="profileLabel">{t('profile.hours')}</span>
            <strong>{quota ? t('profile.freeInWindow', { remaining: remainingHours, limit: limitHours, hours: quotaWindowHours }) : t('profile.freeQuota')}</strong>
            <div className="profileQuotaReadout">
              <span>{remainingHours}</span>
              <small>{t('profile.quotaReadout.limit', { limit: limitHours })}</small>
            </div>
            <div className="profileMeter" aria-hidden="true"><span style={{ width: `${quotaUsedPercent}%` }} /></div>
            <em>{t('profile.quotaDetail', { paid: paidHours, reset: quotaResetLabel, lifetime: lifetimeHours })}</em>
          </div>
          {availableHourPacks.length > 0 && (
            <div className="packButtons">
              {availableHourPacks.map((pack) => (
                <button className="primaryButton" key={pack} disabled={busyPack != null} onClick={() => void checkout(pack)}>
                  {busyPack === pack ? <Loader2 size={16} className="spin" /> : <Wallet size={16} />} {pack}h
                </button>
              ))}
            </div>
          )}
          {availableHourPacks.length === 0 && <span className="profileMutedBadge">{t('profile.paidPacksUnavailable')}</span>}
        </div>
        <div className={projectSlotsFull ? 'profileCard profileActionCard profileSlotsCard full' : 'profileCard profileActionCard profileSlotsCard'}>
          <div>
            <span className="profileLabel">{t('profile.projects')}</span>
            <strong>{projectQuota ? t('profile.projectSlots', { used: projectQuota.used, limit: projectQuota.limit }) : t('profile.projectQuota')}</strong>
            {projectQuota && (
              <span className={projectSlotsFull ? 'profileQuotaStatus full' : 'profileQuotaStatus'}>
                {projectSlotsFull && <CircleAlert size={13} />}
                {projectSlotsFull ? t('profile.projectSlotsFull') : t('profile.projectSlotsAvailable', { remaining: projectQuota.remaining })}
              </span>
            )}
            {projectQuota && <div className="profileMeter compact" aria-hidden="true"><span style={{ width: `${projectSlotPercent}%` }} /></div>}
            <em>{projectSlotsFull ? t('profile.projectSlotFullDetail') : t('profile.projectSlotDetail', { paid: projectQuota?.paidSlots ?? 0, reset: projectQuota?.nextExpiresAt ? ` · ${t('common.nextReset', { date: formatShortDate(projectQuota.nextExpiresAt, locale) })}` : '' })}</em>
          </div>
          {projectQuotaPurchasable && (
            <button className="primaryButton" disabled={busyPack != null} onClick={() => void checkoutProjectSlot()}>
              {busyPack === -1 ? <Loader2 size={16} className="spin" /> : <FolderOpen size={16} />} {t('profile.addSlot')}
            </button>
          )}
          {!projectQuotaPurchasable && <span className="profileMutedBadge">{t('profile.projectSlotsUnavailable')}</span>}
        </div>
        <div className="profileCard profileActionCard">
          <div>
            <span className="profileLabel">{t('profile.session')}</span>
            <strong>{t('profile.signedIn')}</strong>
            <em>{t('profile.sessionBody')}</em>
          </div>
          <button className="ghostButton" onClick={() => fetch('/api/auth/logout', { method: 'POST' }).then(() => location.reload())}>
            <LogOut size={16} /> {t('auth.signOut')}
          </button>
        </div>
        <div className="profileCard profileActionCard profileDangerCard">
          <div>
            <span className="profileLabel">{t('profile.dangerZone')}</span>
            <strong>{t('profile.deleteAllTitle')}</strong>
            <em>{t('profile.deleteAllBody')}</em>
          </div>
          <button className="dangerButton" disabled={deletingAll} onClick={() => setDeleteAllOpen(true)}>
            {deletingAll ? <Loader2 size={16} className="spin" /> : <Trash2 size={16} />} {t('deleteAll.button')}
          </button>
        </div>
      </div>
      {archives.length > 0 && (
        <div className="profileArchives">
          <div className="adminCardHeader compactHeader">
            <h3>{t('profile.archives.title')}</h3>
            <p>{t('profile.archives.body')}</p>
          </div>
          <div className="archiveList">
            {archives.map((archive) => {
              const githubRepoUrl = archive.githubRepoUrl;
              return (
                <div className={`archiveRow ${archive.status}`} key={archive.id}>
                  <div>
                    <strong>{archive.projectTitle}</strong>
                    <em>{statusLabel(archive.status, t)}{archive.expiresAt ? ` · ${t('common.expires', { date: formatShortDate(archive.expiresAt, locale) })}` : ''}</em>
                    {archive.error && <small>{archive.error}</small>}
                  </div>
                  {githubRepoUrl && <a className="ghostButton" href={githubRepoUrl} target="_blank" rel="noopener noreferrer"><ExternalLink size={15} /> {t('common.github')}</a>}
                  {archive.downloadUrl && <a className="primaryButton" href={archive.downloadUrl}><FolderOpen size={15} /> {t('common.zip')}</a>}
                </div>
              );
            })}
          </div>
        </div>
      )}
      <div className="profileMailbox">
        <div className="adminCardHeader compactHeader">
          <h3>{t('profile.mailbox.title')}</h3>
          <p>{t('profile.mailbox.body')}</p>
        </div>
        <div className="profileMessageList" ref={profileMessagesRef}>
          {orderedMessages.map((message) => (
            <div className={`profileMessage ${message.sender} ${message.severity}`} key={message.id}>
              <div className="messageMeta">
                <span>{message.sender === 'user' ? t('common.you') : t('common.system')}</span>
                <time dateTime={message.createdAt}>{formatMessageTime(message.createdAt, locale)}</time>
              </div>
              <p>{message.body}</p>
              {message.dismissedAt && <em>{t('profile.dismissed')}</em>}
            </div>
          ))}
          {orderedMessages.length === 0 && <div className="emptyPool">{t('profile.noMessages')}</div>}
        </div>
        <div className="supportComposer">
          <textarea className="adminTextarea compactTextarea" rows={1} value={supportBody} onChange={(event) => setSupportBody(event.target.value)} onKeyDown={handleSupportKeyDown} placeholder={t('profile.support.placeholder')} />
          <button className="primaryButton" disabled={!supportBody.trim() || sendingSupport} onClick={() => void sendSupportMessage()} aria-label={t('profile.support.sendAria')}>
            {sendingSupport ? <Loader2 size={16} className="spin" /> : <Send size={16} />} {t('profile.support.send')}
          </button>
        </div>
      </div>
      {deleteAllOpen && (
        <DeleteAllAccountDialog
          email={me.user?.email ?? ''}
          busy={deletingAll}
          onCancel={() => setDeleteAllOpen(false)}
          onConfirm={(email) => void deleteAll(email)}
        />
      )}
    </section>
  );
}

const PROFILE_TRANSIENT_NOTICE_TTL_MS = 10 * 60_000;

function isTransientProfileMessage(message: UserNotice, now: number): boolean {
  if (message.sender !== 'system') return false;
  if (message.body.startsWith('Project quota:')) return true;
  if (!message.body.startsWith('Project deletion started:')) return false;
  if (message.dismissedAt) return true;
  const created = Date.parse(message.createdAt);
  return !Number.isNaN(created) && now - created > PROFILE_TRANSIENT_NOTICE_TTL_MS;
}
