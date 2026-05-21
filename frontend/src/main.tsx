import React, { useEffect, useMemo, useRef, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { BookOpen, ChevronDown, CircleHelp, CircleStop, ExternalLink, FolderOpen, ImagePlus, Languages, LayoutPanelLeft, Loader2, LogOut, Minimize2, MessageSquare, Paperclip, Send, Settings, Sparkles, UserRound, X } from 'lucide-react';
import './styles.css';
import { Admin } from './admin';
import { api } from './api';
import { AgentNotificationRow, AppDialog, CanvasLoader, ConfirmDeleteProject, ConfirmExportProject, ConfirmNewProject, DeleteAllAccountDialog, EmptyCanvas, HelpPanel, OnboardingGallery, ProjectList, ServicePanel, UserMessageRow } from './builder_components';
import { BASIC_CHAT_COLLAPSED_KEY, BASIC_CHAT_HEIGHT_KEY, BUILDER_MODE_KEY, COLLAPSED_CHAT_POSITION_KEY, MAX_ATTACHMENTS, SINGLE_VIEW_QUERY } from './config';
import type { AppDialogConfig, BuilderMode, BusyPolicy, Feed, FeedRow, HourQuota, Message, Me, PendingAttachment, PreviewStatus, Project, ProjectArchiveResponse, ProjectExportResponse, ProjectListResponse, ProjectService, UserNotice } from './domain';
import { agentResponseDelayStatus, feedAwaitingAgent, feedHasAssistantAfterLatestUser, feedLiveIdle, feedProjectUpdatedAfterLatestUser, feedRows } from './feed';
import { formatBillingDuration, formatMessageTime, formatResetCountdown } from './format';
import { I18nProvider, resetCountdownLabels, statusLabel, type TranslationKey, useDocumentTitle, useI18n } from './i18n';
import { installPwa } from './pwa';
import { ProfilePanel } from './profile_panel';
import { clampBasicChatHeight, currentViewportHeight, currentViewportWidth, defaultBasicChatHeight, installViewportCssVars, singleViewScreen } from './viewport';

installPwa();
installPlatformFlags();

const LOCAL_AGENT_RUN_MAX_MS = 30 * 60_000;
const LOCAL_AGENT_IDLE_GRACE_MS = 10_000;
const COLLAPSED_CHAT_EDGE_MARGIN = 28;
const ONBOARDING_DISMISSED_KEY = 'likeable.onboarding.dismissedProjects';
const PULL_REFRESH_TRIGGER = 74;
const PULL_REFRESH_MAX = 118;
const PULL_REFRESH_SETTLE_DELAY_MS = 180;
const PULL_REFRESH_TIMEOUT_MS = 3500;
const PENDING_MESSAGE_RECONCILE_MS = 2 * 60_000;
const BASIC_CHAT_MIN_PERSISTED_HEIGHT = 260;
const STALE_PROJECT_DELETION_NOTICE_MS = 10 * 60_000;
const MATRIX_RAIN_COLUMNS = [
  ['10101010', '01010101', '11010110', '00101011', '10110100', '01011001', '11101010', '00110101', '10010110', '01101011'],
  ['01011010', '10100101', '01101001', '10010110', '01010111', '11010010', '00101101', '10110101', '01001011', '11100100'],
  ['11001010', '00110101', '10101001', '01011010', '11100101', '00101011', '10011100', '01110101', '10100110', '01011001']
].map((lines) => `${lines.join('\n')}\n${lines.join('\n')}`);

function StatusMark({ working = false }: { working?: boolean }) {
  return (
    <span className={`mark small statusMark ${working ? 'working' : ''}`}>
      {working && (
        <span className="markMatrixRain" aria-hidden="true">
          {MATRIX_RAIN_COLUMNS.map((column, index) => (
            <span key={index}>{column}</span>
          ))}
        </span>
      )}
      <span className="markGlyph">L</span>
      <span className="brandStatusDot" />
    </span>
  );
}

function googleAuthStartPath() {
  if (typeof navigator === 'undefined') return '/api/auth/google/start';
  const userAgent = navigator.userAgent.toLowerCase();
  const inAppBrowser = [
    'telegram',
    'fbav',
    'fban',
    'instagram',
    'line/',
    'micromessenger',
    'tiktok',
    'twitter',
    'snapchat'
  ].some((marker) => userAgent.includes(marker));
  return inAppBrowser ? '/api/auth/google/start?browser_hint=in_app' : '/api/auth/google/start';
}

function devAuthStartPath(me: Me) {
  const email = me.auth?.devEmail?.trim() || 'admin@example.com';
  return `/api/dev/login?email=${encodeURIComponent(email)}`;
}

function devAuthIsPrimary(me: Me, googleReady: boolean) {
  return Boolean(me.auth?.devAuth && !googleReady);
}

async function startDevAuth(me: Me) {
  const response = await fetch(devAuthStartPath(me), { method: 'POST' });
  if (!response.ok) {
    throw new Error('dev sign in failed');
  }
  location.reload();
}

function installPlatformFlags() {
  if (typeof navigator === 'undefined' || typeof document === 'undefined') return;
  const syncStandalone = () => {
    document.documentElement.dataset.standalone = isStandaloneDisplayMode() ? 'true' : 'false';
  };
  document.documentElement.dataset.android = isAndroidPlatform() ? 'true' : 'false';
  syncStandalone();
  const standaloneQuery = window.matchMedia?.('(display-mode: standalone)');
  standaloneQuery?.addEventListener('change', syncStandalone);
}

function isAndroidPlatform() {
  return typeof navigator !== 'undefined' && /\bAndroid\b/i.test(navigator.userAgent);
}

function isStandaloneDisplayMode() {
  if (typeof window === 'undefined' || typeof navigator === 'undefined') return false;
  const navigatorWithStandalone = navigator as Navigator & { standalone?: boolean };
  return window.matchMedia?.('(display-mode: standalone)').matches || navigatorWithStandalone.standalone === true;
}

function isAndroidBrowserPlatform() {
  return isAndroidPlatform() && !isStandaloneDisplayMode();
}

function App() {
  const [me, setMe] = useState<Me | null>(null);
  const [route, setRoute] = useState(location.pathname);

  useEffect(() => {
    return installViewportCssVars();
  }, []);

  useEffect(() => {
    api<Me>('/api/me').then(setMe).catch(() => setMe({ user: null }));
    const onPop = () => setRoute(location.pathname);
    addEventListener('popstate', onPop);
    return () => removeEventListener('popstate', onPop);
  }, []);

  const nav = (to: string) => {
    history.pushState(null, '', to);
    setRoute(to);
  };

  const { t } = useI18n();
  useDocumentTitle(me ? null : t('app.title'));

  if (!me) return <div className="loading">{t('app.loading')}</div>;

  return (
    <Shell me={me} route={route} nav={nav}>
      {route.startsWith('/admin') && me.isAdmin ? <Admin /> : <Builder nav={nav} me={me} profileRoute={route.startsWith('/profile')} />}
    </Shell>
  );
}

function Shell({ me, route, nav, children }: { me: Me; route: string; nav: (to: string) => void; children: React.ReactNode }) {
  const { t } = useI18n();
  const [notices, setNotices] = useState<UserNotice[]>(me.notices ?? []);
  const [noticeClock, setNoticeClock] = useState(Date.now());
  const online = useOnlineStatus();
  const googleReady = me.auth?.googleConfigured !== false;
  const useDevAuthPrimary = devAuthIsPrimary(me, googleReady);
  const googleAuthHref = googleAuthStartPath();
  useEffect(() => setNotices(me.notices ?? []), [me.notices]);
  useEffect(() => {
    const timer = setInterval(() => setNoticeClock(Date.now()), 60_000);
    return () => clearInterval(timer);
  }, []);
  const visibleNotices = useMemo(() => visibleShellNotices(notices, noticeClock), [notices, noticeClock]);
  const notice = visibleNotices[0];
  const navigate = (to: string) => (event: React.MouseEvent<HTMLAnchorElement>) => {
    event.preventDefault();
    nav(to);
  };
  const navClass = (to: string) => {
    const active = to === '/'
      ? route === '/'
      : route === to || route.startsWith(`${to}/`);
    return active ? 'active' : undefined;
  };
  const ariaCurrent = (to: string) => (navClass(to) ? 'page' : undefined);
  const dismissNotice = async (noticeID: string) => {
    setNotices((current) => current.filter((candidate) => candidate.id !== noticeID));
    try {
      await api(`/api/messages/${noticeID}/dismiss`, { method: 'POST' });
    } catch {
      // The banner is intentionally optimistic; the next /api/me refresh will reconcile.
    }
  };
  return (
    <div className="shell">
      <header className="topbar">
        <button className="brand" onClick={() => nav('/')} aria-label={t('builder.brand.tooltip')}>
          <StatusMark />
        </button>
        <nav>
          <a className={navClass('/')} href="/" onClick={navigate('/')} aria-current={ariaCurrent('/')}>
            <MessageSquare size={18} /> {t('nav.builder')}
          </a>
          {me.user && (
            <a className={navClass('/profile')} href="/profile" onClick={navigate('/profile')} aria-current={ariaCurrent('/profile')}>
              <UserRound size={18} /> {t('nav.profile')}
            </a>
          )}
          {me.isAdmin && (
            <a className={navClass('/admin')} href="/admin" onClick={navigate('/admin')} aria-current={ariaCurrent('/admin')}>
              <Settings size={18} /> {t('nav.admin')}
            </a>
          )}
        </nav>
        <div className="account">
          <LanguageToggle />
          {me.user ? (
            <>
              <span>{me.user.email}</span>
              <button onClick={() => fetch('/api/auth/logout', { method: 'POST' }).then(() => location.reload())}><LogOut size={17} /></button>
            </>
          ) : (
            <>
              {useDevAuthPrimary ? (
                <button onClick={() => void startDevAuth(me)}>{t('auth.signIn')}</button>
              ) : (
                <>
                  <a className={!googleReady ? 'disabled' : ''} href={googleAuthHref}>{t('auth.signIn')}</a>
                  {me.auth?.devAuth && <button onClick={() => void startDevAuth(me)}>{t('auth.dev')}</button>}
                </>
              )}
            </>
          )}
        </div>
      </header>
      {(!online || notice) && (
        <div className="noticeStack">
          {!online && (
            <div className="systemNotice warning">
              <strong>{t('notice.offlineTitle')}</strong>
              <span>{t('notice.offlineBody')}</span>
            </div>
          )}
          {notice && (
            <div className={`systemNotice ${notice.severity}`}>
              <strong>{t('notice.system')}</strong>
              <span>{notice.body}</span>
              <button onClick={() => void dismissNotice(notice.id)} aria-label={t('notice.dismiss')}><X size={14} /></button>
            </div>
          )}
        </div>
      )}
      <main className="workspace">{children}</main>
    </div>
  );
}

function LanguageToggle({ className = '' }: { className?: string }) {
  const { locale, setLocale, t } = useI18n();
  const nextLocale = locale === 'en' ? 'uk' : 'en';
  const currentLanguage = locale === 'en' ? t('language.english') : t('language.ukrainian');
  const tip = t('language.current', { language: currentLanguage });
  return (
    <button
      className={className || 'languageToggle'}
      onClick={() => setLocale(nextLocale)}
      aria-label={t('language.switch')}
      data-tip={tip}
    >
      <Languages size={16} />
      <span>{locale === 'en' ? 'UA' : 'EN'}</span>
    </button>
  );
}

function useOnlineStatus() {
  const [online, setOnline] = useState(() => navigator.onLine);

  useEffect(() => {
    const handleOnline = () => setOnline(true);
    const handleOffline = () => setOnline(false);
    addEventListener('online', handleOnline);
    addEventListener('offline', handleOffline);
    return () => {
      removeEventListener('online', handleOnline);
      removeEventListener('offline', handleOffline);
    };
  }, []);

  return online;
}

function Builder({ nav, me, profileRoute = false }: { nav: (to: string) => void; me: Me; profileRoute?: boolean }) {
  const { locale, t } = useI18n();
  const signedIn = Boolean(me.user);
  const userID = me.user?.id ?? '';
  const googleReady = me.auth?.googleConfigured !== false;
  const useDevAuthPrimary = devAuthIsPrimary(me, googleReady);
  const googleAuthHref = googleAuthStartPath();
  const [projects, setProjects] = useState<Project[]>([]);
  const [activeID, setActiveID] = useState<string>('');
  const [feed, setFeed] = useState<Feed | null>(null);
  const [prompt, setPrompt] = useState('');
  const [busy, setBusy] = useState(false);
  const [messageSubmitting, setMessageSubmitting] = useState(false);
  const [promptImproving, setPromptImproving] = useState(false);
  const [projectsLoaded, setProjectsLoaded] = useState(false);
  const [projectsLoadedUserID, setProjectsLoadedUserID] = useState('');
  const [pendingAgentRuns, setPendingAgentRuns] = useState<Record<string, number>>({});
  const [pendingMessagesByProject, setPendingMessagesByProject] = useState<Record<string, Message[]>>({});
  const [projectCap, setProjectCap] = useState<number | null>(null);
  const [showProjects, setShowProjects] = useState(false);
  const [showProfile, setShowProfile] = useState(profileRoute && signedIn);
  const [showHelp, setShowHelp] = useState(false);
  const [showServices, setShowServices] = useState(false);
  const [manualOnboardingOpen, setManualOnboardingOpen] = useState(false);
  const [dismissedOnboardingProjects, setDismissedOnboardingProjects] = useState<Record<string, boolean>>(() => readDismissedOnboardingProjects());
  const [confirmNewProject, setConfirmNewProject] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<Project | null>(null);
  const [exportTarget, setExportTarget] = useState<Project | null>(null);
  const [exportingID, setExportingID] = useState('');
  const [exportingMode, setExportingMode] = useState<'github' | 'zip' | ''>('');
  const [controllingProjectID, setControllingProjectID] = useState('');
  const [dialog, setDialog] = useState<AppDialogConfig | null>(null);
  const [iframeLoaded, setIframeLoaded] = useState(false);
  const [previewStatus, setPreviewStatus] = useState<PreviewStatus | null>(null);
  const [attachments, setAttachments] = useState<PendingAttachment[]>([]);
  const [draggingFiles, setDraggingFiles] = useState(false);
  const [basicChatCollapsed, setBasicChatCollapsed] = useState(() => localStorage.getItem(BASIC_CHAT_COLLAPSED_KEY) === 'true');
  const [basicChatHeight, setBasicChatHeight] = useState(() => {
    const stored = Number(localStorage.getItem(BASIC_CHAT_HEIGHT_KEY));
    if (Number.isFinite(stored) && stored > 0) {
      return isAndroidPlatform() ? stored : clampBasicChatHeight(stored);
    }
    return defaultBasicChatHeight();
  });
  const [collapsedChatPosition, setCollapsedChatPosition] = useState(() => initialCollapsedChatPosition());
  const [busyPolicy, setBusyPolicy] = useState<BusyPolicy>('queue');
  const [hourQuota, setHourQuota] = useState<HourQuota | null>(me.hourQuota ?? null);
  const [quotaNow, setQuotaNow] = useState(Date.now());
  const textareaRef = useRef<HTMLTextAreaElement | null>(null);
  const messagesRef = useRef<HTMLDivElement | null>(null);
  const fileInputRef = useRef<HTMLInputElement | null>(null);
  const dragDepthRef = useRef(0);
  const [singleView, setSingleView] = useState(singleViewScreen);
  const [mode, setMode] = useState<BuilderMode>(() => {
    return localStorage.getItem(BUILDER_MODE_KEY) === 'split' ? 'split' : 'overlay';
  });
  const viewMode: BuilderMode = singleView ? 'overlay' : mode;
  const projectsLoadedForCurrentUser = signedIn && projectsLoaded && projectsLoadedUserID === userID;
  const currentProjects = projectsLoadedForCurrentUser ? projects : [];
  const active = useMemo(() => currentProjects.find((p) => p.id === activeID), [currentProjects, activeID]);
  const activeProjectCandidate = feed?.project?.id === activeID ? feed.project : active;
  const activeProjectInCurrentList = Boolean(activeProjectCandidate?.id && currentProjects.some((project) => project.id === activeProjectCandidate.id));
  const activeProject = projectsLoadedForCurrentUser && activeProjectInCurrentList ? activeProjectCandidate : undefined;
  const activePendingMessages = activeID ? pendingMessagesByProject[activeID] ?? [] : [];
  const displayFeed = useMemo(() => feedWithPendingMessages(feed, activeProject, activeID, activePendingMessages), [feed, activeProject, activeID, activePendingMessages]);
  const selectedService = useMemo(() => selectedProjectService(activeProject), [activeProject]);
  const activePreviewURL = selectedService?.url ?? activeProject?.previewUrl ?? '';
  const rawRows = useMemo(() => feedRows(displayFeed, quotaNow), [displayFeed, quotaNow]);
  const rows = useMemo(() => normalizeActiveNotificationRows(rawRows), [rawRows]);
  const agentDelayStatus = useMemo(() => agentResponseDelayStatus(displayFeed, quotaNow), [displayFeed, quotaNow]);
  const pendingAgentStartedAt = activeProject?.id ? pendingAgentRuns[activeProject.id] : undefined;
  const pendingAgentAge = typeof pendingAgentStartedAt === 'number' ? Date.now() - pendingAgentStartedAt : null;
  const localAgentRunActive = Boolean(
    pendingAgentAge != null
    && pendingAgentAge < LOCAL_AGENT_RUN_MAX_MS
    && !feedHasAssistantAfterLatestUser(displayFeed)
    && !feedProjectUpdatedAfterLatestUser(displayFeed)
    && (pendingAgentAge < LOCAL_AGENT_IDLE_GRACE_MS || !feedLiveIdle(displayFeed))
  );
  const agentWorking = Boolean(signedIn && activeProject?.status === 'ready' && activePreviewURL && (messageSubmitting || localAgentRunActive || displayFeed?.live?.isProcessing || feedAwaitingAgent(displayFeed) || agentDelayStatus?.active));
  const agentWorkingLabel = messageSubmitting ? t('builder.agent.transmitting') : t('builder.agent.synthesizing');
  const agentDelayLabel = agentDelayStatus?.phase === 'stalled' ? t('builder.agent.noActivity') : t('builder.agent.waiting');
  const lastRow = rows.at(-1);
  const lastRowSignature = lastRow ? `${lastRow.id}:${lastRow.body}` : '';
  const modeLabel = viewMode === 'overlay' ? t('builder.mode.basic') : t('builder.mode.split');
  const quotaProjectCount = currentProjects.filter((project) => project.status !== 'archived' && project.status !== 'deleting').length;
  const projectCapLabel = projectCap == null ? `${quotaProjectCount}` : `${quotaProjectCount}/${projectCap}`;
  const projectCapReached = signedIn && projectCap != null && quotaProjectCount >= projectCap;
  const selectedServiceLabel = selectedService?.name ?? '--';
  const hasMultipleServices = (activeProject?.services?.length ?? 0) > 1;
  const projectUpdatedTime = activeProject?.updatedAt ? formatMessageTime(activeProject.updatedAt, locale) : '';
  const projectChromeMeta = activeProject
    ? projectUpdatedTime
      ? t('builder.projectMeta.updated', { status: statusLabel(activeProject.status, t), time: projectUpdatedTime })
      : statusLabel(activeProject.status, t)
    : '';
  const hourQuotaLabel = hourQuota ? `${formatBillingDuration(hourQuota.remainingMs, resetCountdownLabels(t))}/${formatBillingDuration(hourQuota.limitMs, resetCountdownLabels(t))}` : '';
  const hourQuotaTooltip = hourQuota ? t('builder.hourQuota.tooltip', { paid: formatBillingDuration(hourQuota.paidRemainingMs ?? 0, resetCountdownLabels(t)), reset: formatResetCountdown(hourQuota.resetsAt, quotaNow, resetCountdownLabels(t)) }) : '';
  const githubConnected = Boolean(me.githubConnected);
  const githubNeedsReconnect = Boolean(me.githubNeedsReconnect);
  const projectArchived = activeProject?.status === 'archived';
  const projectsLoading = signedIn && !projectsLoadedForCurrentUser;
  const noActiveProject = signedIn && projectsLoadedForCurrentUser && !activeProject;
  const isProjectStarting = activeProject?.status === 'creating' || activeProject?.status === 'launching';
  const previewRuntimeActive = activeProject?.status === 'ready';
  const previewMaintenance = Boolean(activePreviewURL && previewRuntimeActive && previewStatus?.maintenance);
  const previewReady = Boolean(activePreviewURL && previewRuntimeActive && previewStatus?.ready);
  const previewDisplayable = Boolean(activePreviewURL && previewRuntimeActive && (previewReady || previewMaintenance));
  const projectHasMessages = (displayFeed?.localMessages?.length ?? 0) > 0 || (displayFeed?.messages?.length ?? 0) > 0;
  const onboardingDismissed = Boolean(activeProject?.id && dismissedOnboardingProjects[activeProject.id]);
  const initialOnboardingEligible = signedIn && Boolean(activeProject) && currentProjects.length <= 1 && !projectHasMessages && !onboardingDismissed && (isProjectStarting || activeProject?.status === 'ready');
  const utilityScreenOpen = showProjects || showProfile || showHelp || showServices;
  const showOnboardingWizard = signedIn && Boolean(activeProject) && (manualOnboardingOpen || (initialOnboardingEligible && !utilityScreenOpen));
  const onboardingReady = Boolean(activeProject?.status === 'ready' && activePreviewURL && previewReady);
  const chatCollapsedForTutorial = viewMode === 'overlay' && showOnboardingWizard;
  const effectiveChatCollapsed = basicChatCollapsed || chatCollapsedForTutorial;
  const canvasStatusLabel = agentWorking ? t('builder.status.agentWorking') : previewMaintenance ? t('builder.status.maintenance') : activeProject?.status === 'ready' ? (previewReady ? t('builder.status.canvasLive') : t('builder.status.canvasStarting')) : isProjectStarting ? t('builder.status.canvasStarting') : projectArchived ? t('builder.status.canvasArchived') : activeProject?.status === 'stopped' ? t('builder.status.canvasStopped') : activeProject?.status === 'error' ? t('builder.status.canvasError') : t('builder.status.canvasIdle');
  const idleStopCountdown = activeProject?.status === 'ready' && activeProject.playgroundIdleStopAt ? formatResetCountdown(activeProject.playgroundIdleStopAt, quotaNow, resetCountdownLabels(t)) : '';
  const idleStopTooltip = idleStopCountdown ? t('builder.idleStop.tooltip', { time: idleStopCountdown }) : '';
  const hasDraft = Boolean(prompt.trim()) || attachments.length > 0;
  const composerDisabled = !signedIn || projectsLoading || noActiveProject || projectArchived;
  const canSend = signedIn && !composerDisabled && hasDraft && !busy && !messageSubmitting && Boolean(activePreviewURL) && (activeProject?.status === 'ready' || previewReady);
  const hasActiveNotification = rows.some((row) => row.kind === 'notification' && row.active);
  const canvasLoading = projectsLoading || isProjectStarting || Boolean(activePreviewURL && previewRuntimeActive && !previewMaintenance && (!previewReady || !iframeLoaded));
  const brandWorking = agentWorking || hasActiveNotification || canvasLoading;
  const agentActivityActive = Boolean(messageSubmitting || localAgentRunActive || displayFeed?.live?.isProcessing || feedAwaitingAgent(displayFeed) || agentDelayStatus?.active || hasActiveNotification);
  const inputPlaceholder = !signedIn
    ? t('builder.placeholder.signIn')
    : projectsLoading
    ? t('builder.placeholder.loadingProjects')
    : noActiveProject
    ? t('builder.placeholder.noProject')
    : isProjectStarting
    ? t('builder.placeholder.starting')
    : projectArchived
      ? t('builder.placeholder.archived')
    : activeProject?.status === 'error'
      ? t('builder.placeholder.error')
      : singleView
        ? t('builder.placeholder.single')
        : t('builder.placeholder.default');
  const pageTitle = showProfile
    ? t('profile.title')
    : showHelp
      ? t('page.about.title')
      : showProjects
        ? t('projects.title')
        : activeProject?.title
          ? projectPageTitle(activeProject.title, selectedService?.name, t)
          : t('app.title');
  const studioState = projectArchived
    ? 'archived'
    : agentActivityActive
      ? 'building'
      : previewMaintenance
        ? 'maintenance'
        : activeProject?.status === 'stopped'
          ? 'stopped'
          : activeProject?.status === 'ready' && previewReady
            ? 'live'
            : isProjectStarting || canvasLoading
              ? 'starting'
              : activeProject?.status === 'error'
                ? 'error'
                : 'idle';
  const studioNextActionLabel = agentActivityActive
    ? t('builder.stopAgent')
    : projectArchived
      ? t('projects.export.title')
      : noActiveProject
        ? t('projects.new')
        : t('builder.nextAction.prompt');
  const studioNowMeta = agentActivityActive
    ? agentWorkingLabel
    : selectedService?.name
      ? `${canvasStatusLabel} · ${selectedService.name}`
      : canvasStatusLabel;
  const StudioNextIcon = agentActivityActive ? CircleStop : projectArchived || noActiveProject ? FolderOpen : Sparkles;
  const composerHintTone = !signedIn || projectsLoading || noActiveProject
    ? 'blocked'
    : isProjectStarting
      ? 'pending'
      : activeProject?.status === 'error'
        ? 'error'
        : activeProject?.status === 'stopped' || projectArchived
          ? 'paused'
          : '';
  const composerModeHint = agentActivityActive
    ? busyPolicy === 'queue'
      ? t('builder.composerHint.agentQueue')
      : t('builder.composerHint.agentSteer')
    : !signedIn
      ? t('builder.composerHint.signIn')
      : projectsLoading
        ? t('builder.composerHint.loadingProjects')
        : noActiveProject
          ? t('builder.composerHint.noProject')
          : isProjectStarting
            ? t('builder.composerHint.starting')
            : activeProject?.status === 'error'
              ? t('builder.composerHint.error')
              : activeProject?.status === 'stopped'
                ? t('builder.composerHint.stopped')
                : projectArchived
                  ? t('builder.composerHint.archived')
                  : t('builder.composerHint.ready');

  useDocumentTitle(pageTitle);

  const updateBasicChatHeight = (next: number | ((height: number) => number)) => {
    setBasicChatHeight((current) => {
      const value = typeof next === 'function' ? next(current) : next;
      const clamped = isAndroidPlatform() ? Math.max(BASIC_CHAT_MIN_PERSISTED_HEIGHT, Math.round(value)) : clampBasicChatHeight(value);
      localStorage.setItem(BASIC_CHAT_HEIGHT_KEY, String(Math.round(clamped)));
      return clamped;
    });
  };

  const loadProjects = () => api<ProjectListResponse>('/api/projects').then((r) => {
    const nextProjects = Array.isArray(r.projects) ? r.projects : [];
    const nextProjectIDs = new Set(nextProjects.map((project) => project.id));
    setProjects(nextProjects);
    setFeed((current) => current?.project?.id && nextProjectIDs.has(current.project.id) ? current : null);
    setProjectCap(typeof r.projectCap === 'number' ? r.projectCap : null);
    setProjectsLoadedUserID(userID);
    setProjectsLoaded(true);
    setActiveID((current) => {
      if (current && nextProjects.some((project) => project.id === current)) return current;
      return nextProjects[0]?.id ?? '';
    });
  });
  const refreshQuota = () => api<Me>('/api/me')
    .then((next) => setHourQuota(next.hourQuota ?? null))
    .catch(() => undefined);
  const rememberPendingAgentRun = (projectID: string) => {
    setPendingAgentRuns((current) => ({ ...current, [projectID]: Date.now() }));
  };
  const forgetPendingAgentRun = (projectID: string) => {
    setPendingAgentRuns((current) => {
      if (!current[projectID]) return current;
      const { [projectID]: _removed, ...rest } = current;
      return rest;
    });
  };

  useEffect(() => {
    if (!signedIn) {
      setProjects([]);
      setActiveID('');
      setFeed(null);
      setProjectsLoaded(false);
      setProjectsLoadedUserID('');
      setPendingAgentRuns({});
      setPendingMessagesByProject({});
      setShowProjects(false);
      setShowProfile(false);
      setShowHelp(false);
      setShowServices(false);
      return;
    }
    setProjectsLoaded(false);
    setProjectsLoadedUserID('');
    void loadProjects().catch(() => {
      setProjects([]);
      setActiveID('');
      setProjectsLoadedUserID(userID);
      setProjectsLoaded(true);
    });
    void refreshQuota();
  }, [signedIn, me.user?.id]);
  useEffect(() => {
    localStorage.setItem(BUILDER_MODE_KEY, mode);
  }, [mode]);
  useEffect(() => {
    document.documentElement.dataset.builderMode = viewMode;
    return () => {
      delete document.documentElement.dataset.builderMode;
    };
  }, [viewMode]);
  useEffect(() => {
    const query = window.matchMedia(SINGLE_VIEW_QUERY);
    const update = () => {
      setSingleView(query.matches);
      if (!isAndroidPlatform()) updateBasicChatHeight((height) => height);
    };
    update();
    query.addEventListener('change', update);
    return () => query.removeEventListener('change', update);
  }, []);
  useEffect(() => {
    localStorage.setItem(BASIC_CHAT_COLLAPSED_KEY, String(basicChatCollapsed));
  }, [basicChatCollapsed]);
  useEffect(() => {
    localStorage.setItem(BASIC_CHAT_HEIGHT_KEY, String(Math.round(basicChatHeight)));
  }, [basicChatHeight]);
  useEffect(() => {
    localStorage.setItem(COLLAPSED_CHAT_POSITION_KEY, JSON.stringify(collapsedChatPosition));
  }, [collapsedChatPosition]);
  useEffect(() => {
    const resize = () => {
      if (!isAndroidPlatform()) updateBasicChatHeight((height) => height);
      setCollapsedChatPosition((position) => clampCollapsedChatPosition(position));
    };
    const visualViewport = window.visualViewport;
    addEventListener('resize', resize);
    addEventListener('orientationchange', resize);
    visualViewport?.addEventListener('resize', resize);
    visualViewport?.addEventListener('scroll', resize);
    return () => {
      removeEventListener('resize', resize);
      removeEventListener('orientationchange', resize);
      visualViewport?.removeEventListener('resize', resize);
      visualViewport?.removeEventListener('scroll', resize);
    };
  }, []);
  useEffect(() => {
    setShowProfile(profileRoute && signedIn);
    if (profileRoute && signedIn) {
      setShowProjects(false);
      setBasicChatCollapsed(false);
    }
  }, [profileRoute, signedIn]);
  useEffect(() => {
    setHourQuota(me.hourQuota ?? null);
  }, [me.hourQuota]);
  useEffect(() => {
    const timer = setInterval(() => setQuotaNow(Date.now()), 30000);
    return () => clearInterval(timer);
  }, []);

  useEffect(() => {
    if (!activeID) return;
    const load = () => api<Feed>(`/api/projects/${activeID}/feed`).then((nextFeed) => {
      setFeed((current) => mergeFeedSnapshot(current, nextFeed));
    }).catch((err) => {
      if (err instanceof Error && err.message.includes('project not found')) {
        setFeed(null);
        void loadProjects();
        return;
      }
      console.error(err);
    });
    void load();
    const timer = setInterval(load, agentWorking ? 1500 : 6000);
    return () => clearInterval(timer);
  }, [activeID, agentWorking]);
  useEffect(() => {
    if (!feed?.project) return;
    setProjects((current) => current.map((project) => project.id === feed.project.id ? feed.project : project));
  }, [feed?.project]);
  useEffect(() => {
    if (!feed?.project?.id) return;
    const serverMessages = feed.localMessages ?? [];
    if (serverMessages.length === 0) return;
    setPendingMessagesByProject((current) => {
      const pending = current[feed.project.id] ?? [];
      const nextPending = pending.filter((message) => !serverMessages.some((serverMessage) => messageMatchesServerCopy(message, serverMessage)));
      if (nextPending.length === pending.length) return current;
      if (nextPending.length === 0) {
        const { [feed.project.id]: _removed, ...rest } = current;
        return rest;
      }
      return { ...current, [feed.project.id]: nextPending };
    });
  }, [feed?.project?.id, feed?.localMessages]);
  useEffect(() => {
    if (!feed?.project) return;
    const pendingStartedAt = pendingAgentRuns[feed.project.id];
    const pendingAgeMs = typeof pendingStartedAt === 'number' ? Date.now() - pendingStartedAt : null;
    const idleSettled = feedLiveIdle(feed) && (pendingAgeMs == null || pendingAgeMs >= LOCAL_AGENT_IDLE_GRACE_MS);
    if (feed.project.status !== 'ready' || idleSettled || feedHasAssistantAfterLatestUser(feed) || feedProjectUpdatedAfterLatestUser(feed)) {
      forgetPendingAgentRun(feed.project.id);
    }
  }, [feed, pendingAgentRuns]);
  useEffect(() => {
    setShowServices(false);
  }, [activeID, selectedService?.name]);
  useEffect(() => {
    const textarea = textareaRef.current;
    if (!textarea) return;
    const maxHeight = singleView ? 112 : 180;
    const minHeight = singleView ? 44 : 50;
    textarea.style.height = 'auto';
    const nextHeight = Math.min(textarea.scrollHeight, maxHeight);
    textarea.style.height = `${Math.max(nextHeight, minHeight)}px`;
    textarea.style.overflowY = textarea.scrollHeight > maxHeight ? 'auto' : 'hidden';
  }, [prompt, viewMode, singleView]);
  useEffect(() => {
    setIframeLoaded(false);
    setPreviewStatus(null);
  }, [activeProject?.id, activePreviewURL, activeProject?.status]);
  useEffect(() => {
    if (!activeProject?.id || activeProject.status === 'deleting' || projectArchived) {
      setPreviewStatus(null);
      return;
    }
    let cancelled = false;
    const load = () => api<PreviewStatus>(`/api/projects/${activeProject.id}/preview-status`)
      .then((status) => {
        if (cancelled) return;
        setPreviewStatus(status);
        const projectSnapshot = status.project;
        if (projectSnapshot) {
          setProjects((current) => current.map((project) => project.id === projectSnapshot.id ? projectSnapshot : project));
          setFeed((current) => current?.project.id === projectSnapshot.id ? { ...current, project: projectSnapshot } : current);
        }
        if (!status.ready && !status.maintenance) setIframeLoaded(false);
        if (status.ready && projectSnapshot?.status !== 'stopped' && activeProject.status !== 'ready') {
          setProjects((current) => current.map((project) => project.id === activeProject.id ? { ...project, status: 'ready', errorMessage: '' } : project));
          setFeed((current) => current?.project.id === activeProject.id ? { ...current, project: { ...current.project, status: 'ready', errorMessage: '' } } : current);
        }
      })
      .catch(() => {
        if (cancelled) return;
        setPreviewStatus({ ready: false, status: 'starting', checkedAt: new Date().toISOString() });
        setIframeLoaded(false);
      });
    void load();
    const timer = setInterval(load, (previewStatus?.ready || previewStatus?.maintenance) && !agentWorking ? 10000 : 3000);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [activeProject?.id, activePreviewURL, activeProject?.status, agentWorking, previewStatus?.maintenance, previewStatus?.ready, projectArchived]);
  useEffect(() => {
    setAttachments([]);
    dragDepthRef.current = 0;
    setDraggingFiles(false);
  }, [activeID]);
  useEffect(() => {
    const messages = messagesRef.current;
    if (!messages) return;
    messages.scrollTop = messages.scrollHeight;
  }, [activeID, lastRowSignature, agentWorking]);

  const createOrSend = async () => {
    if (!signedIn) return;
    if (!activeProject) return;
    const text = prompt.trim();
    const files = attachments;
    if (!text && files.length === 0) return;
    const optimisticID = `optimistic-${crypto.randomUUID()}`;
    const optimisticMessage: Message = {
      id: optimisticID,
      role: 'user',
      body: text,
      createdAt: new Date().toISOString(),
      attachments: files.map((attachment) => ({
        id: attachment.id,
        filename: attachment.file.name,
        contentType: attachment.file.type,
        size: attachment.file.size
      }))
    };
    setPendingMessagesByProject((current) => addPendingMessage(current, activeProject.id, optimisticMessage));
    setPrompt('');
    setAttachments([]);
    setBusy(true);
    setMessageSubmitting(true);
    try {
      let response: { message?: Message };
      if (files.length > 0) {
        const form = new FormData();
        form.append('text', text);
        form.append('busy_policy', busyPolicy);
        files.forEach((attachment) => form.append('attachments', attachment.file, attachment.file.name));
        response = await api<{ message?: Message }>(`/api/projects/${activeProject.id}/messages`, { method: 'POST', body: form });
      } else {
        response = await api<{ message?: Message }>(`/api/projects/${activeProject.id}/messages`, { method: 'POST', body: JSON.stringify({ text, busy_policy: busyPolicy }) });
      }
      const acceptedMessage = response.message;
      if (acceptedMessage) {
        setPendingMessagesByProject((current) => replacePendingMessage(current, activeProject.id, optimisticID, acceptedMessage));
      }
      rememberPendingAgentRun(activeProject.id);
      try {
        setFeed(await api<Feed>(`/api/projects/${activeProject.id}/feed`));
        setPendingMessagesByProject((current) => removePendingMessage(current, activeProject.id, response.message?.id ?? optimisticID));
      } catch (err) {
        console.error(err);
      }
      void refreshQuota();
    } catch (err) {
      setPendingMessagesByProject((current) => removePendingMessage(current, activeProject.id, optimisticID));
      setPrompt(text);
      setAttachments(files);
      setDialog({ title: t('dialog.requestFailed.title'), body: err instanceof Error ? err.message : t('dialog.requestFailed.title'), confirmLabel: t('common.close') });
    } finally {
      setMessageSubmitting(false);
      setBusy(false);
    }
  };
  const interruptAgent = async () => {
    if (!signedIn || !activeProject) return;
    setBusy(true);
    try {
      await api(`/api/projects/${activeProject.id}/agent/interrupt`, { method: 'POST', body: JSON.stringify({}) });
      forgetPendingAgentRun(activeProject.id);
      setMessageSubmitting(false);
      setFeed(await api<Feed>(`/api/projects/${activeProject.id}/feed`));
    } catch (err) {
      setDialog({ title: t('dialog.stopFailed.title'), body: err instanceof Error ? err.message : t('dialog.requestFailed.title'), tone: 'warning', confirmLabel: t('common.close') });
    } finally {
      setBusy(false);
    }
  };
  const createProject = async (title?: string) => {
    if (!signedIn) return;
    setBusy(true);
    try {
      const trimmedTitle = title?.trim();
      const res = await api<{ project: Project }>('/api/projects', { method: 'POST', body: JSON.stringify({ confirm: true, title: trimmedTitle || undefined }) });
      setConfirmNewProject(false);
      setShowProjects(false);
      setShowProfile(false);
      setShowServices(false);
      await loadProjects();
      void refreshQuota();
      setActiveID(res.project.id);
      setFeed({ project: res.project, localMessages: [], messages: [], activity: [], live: null });
      nav('/');
    } catch (err) {
      setDialog({ title: t('dialog.projectFailed.title'), body: err instanceof Error ? err.message : t('dialog.requestFailed.title'), tone: 'warning', confirmLabel: t('common.close') });
    } finally {
      setBusy(false);
    }
  };
  const renameProject = async (project: Project, title: string) => {
    if (!signedIn) return;
    const trimmedTitle = title.trim();
    if (!trimmedTitle || trimmedTitle === project.title) return;
    setBusy(true);
    try {
      const res = await api<{ project: Project }>(`/api/projects/${project.id}`, { method: 'PATCH', body: JSON.stringify({ title: trimmedTitle }) });
      setProjects((current) => current.map((item) => item.id === project.id ? res.project : item));
      setFeed((current) => current?.project.id === project.id ? { ...current, project: res.project } : current);
    } catch (err) {
      setDialog({ title: t('dialog.renameFailed.title'), body: err instanceof Error ? err.message : t('dialog.requestFailed.title'), tone: 'warning', confirmLabel: t('common.close') });
    } finally {
      setBusy(false);
    }
  };
  const selectService = async (service: ProjectService) => {
    setShowServices(false);
    if (!signedIn || !activeProject || service.name === activeProject.selectedServiceName) return;
    setBusy(true);
    try {
      const res = await api<{ project: Project }>(`/api/projects/${activeProject.id}`, { method: 'PATCH', body: JSON.stringify({ selectedServiceName: service.name }) });
      setPreviewStatus(null);
      setIframeLoaded(false);
      setProjects((current) => current.map((item) => item.id === res.project.id ? res.project : item));
      setFeed((current) => current?.project.id === res.project.id ? { ...current, project: res.project } : current);
    } catch (err) {
      setDialog({ title: t('dialog.serviceSwitchFailed.title'), body: err instanceof Error ? err.message : t('dialog.requestFailed.title'), tone: 'warning', confirmLabel: t('common.close') });
    } finally {
      setBusy(false);
    }
  };
  const deleteProject = async () => {
    if (!signedIn || !deleteTarget) return;
    const targetID = deleteTarget.id;
    setBusy(true);
    try {
      await api(`/api/projects/${targetID}`, { method: 'DELETE' });
      forgetPendingAgentRun(targetID);
      setPendingMessagesByProject((current) => {
        const { [targetID]: _removed, ...rest } = current;
        return rest;
      });
      const remaining = projects.filter((project) => project.id !== targetID);
      setProjects(remaining);
      setProjectCap((cap) => cap);
      setDeleteTarget(null);
      void refreshQuota();
      if (activeID === targetID) {
        setActiveID(remaining[0]?.id ?? '');
        setFeed(null);
      }
    } catch (err) {
      setDialog({ title: t('dialog.deleteFailed.title'), body: err instanceof Error ? err.message : t('dialog.requestFailed.title'), tone: 'danger', confirmLabel: t('common.close') });
    } finally {
      setBusy(false);
    }
  };
  const controlProjectPlayground = async (project: Project, action: 'start' | 'stop' | 'restart') => {
    if (!signedIn) return;
    setBusy(true);
    setControllingProjectID(project.id);
    try {
      const res = await api<{ project: Project }>(`/api/projects/${project.id}/playground`, { method: 'POST', body: JSON.stringify({ action }) });
      setPreviewStatus(null);
      setIframeLoaded(false);
      setProjects((current) => current.map((item) => item.id === project.id ? res.project : item));
      setFeed((current) => current?.project.id === project.id ? { ...current, project: res.project } : current);
    } catch (err) {
      setDialog({ title: t('dialog.playgroundActionFailed.title'), body: err instanceof Error ? err.message : t('dialog.requestFailed.title'), tone: 'warning', confirmLabel: t('common.close') });
    } finally {
      setControllingProjectID('');
      setBusy(false);
    }
  };
  const requestProjectExport = (project: Project) => {
    setExportTarget(project);
  };
  const connectGithub = () => {
    location.href = '/api/profile/github/start';
  };
  const exportProject = async (project: Project, repoName: string, privateRepo: boolean) => {
    if (!signedIn) return;
    setBusy(true);
    setExportingID(project.id);
    setExportingMode('github');
    try {
      const res = await api<ProjectExportResponse>(`/api/projects/${project.id}/export`, {
        method: 'POST',
        body: JSON.stringify({ repoName, private: privateRepo })
      });
      setExportTarget(null);
      setDialog({
        title: t('dialog.exportReady.title'),
        body: t('dialog.exportReady.bodyProject', { title: project.title, url: res.githubRepoUrl }),
        confirmLabel: t('dialog.exportReady.openGitHub'),
        onConfirm: () => {
          window.open(res.githubRepoUrl, '_blank', 'noopener,noreferrer');
        }
      });
    } catch (err) {
      setExportTarget(null);
      setDialog({ title: t('dialog.exportFailed.title'), body: err instanceof Error ? err.message : t('dialog.requestFailed.title'), tone: 'warning', confirmLabel: t('common.close') });
    } finally {
      setExportingID('');
      setExportingMode('');
      setBusy(false);
    }
  };
  const exportProjectZip = async (project: Project) => {
    if (!signedIn) return;
    setBusy(true);
    setExportingID(project.id);
    setExportingMode('zip');
    try {
      const res = await api<ProjectArchiveResponse>(`/api/projects/${project.id}/archive`, {
        method: 'POST',
        body: JSON.stringify({})
      });
      setExportTarget(null);
      location.href = res.downloadUrl;
    } catch (err) {
      setExportTarget(null);
      setDialog({ title: t('dialog.zipExportFailed.title'), body: err instanceof Error ? err.message : t('dialog.requestFailed.title'), tone: 'warning', confirmLabel: t('common.close') });
    } finally {
      setExportingID('');
      setExportingMode('');
      setBusy(false);
    }
  };
  const handleComposerKeyDown = (event: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (singleView) return;
    if (event.key !== 'Enter' || event.shiftKey || event.nativeEvent.isComposing) return;
    event.preventDefault();
    if (canSend) void createOrSend();
  };
  const applyImprovedPrompt = (nextPrompt: string) => {
    setPrompt(nextPrompt);
    requestAnimationFrame(() => {
      const textarea = textareaRef.current;
      textarea?.focus();
      textarea?.setSelectionRange(nextPrompt.length, nextPrompt.length);
    });
  };
  const improveCurrentPrompt = async () => {
    if (!activeProject || promptImproving) return;
    const draft = prompt;
    setPromptImproving(true);
    try {
      const response = await api<{ text: string; source?: string; warning?: string }>(`/api/projects/${activeProject.id}/prompt-improve`, {
        method: 'POST',
        body: JSON.stringify({ text: draft, locale })
      });
      applyImprovedPrompt(response.text?.trim() || improvePromptDraft(draft, activeProject.title, t));
    } catch (err) {
      console.error(err);
      applyImprovedPrompt(improvePromptDraft(draft, activeProject.title, t));
    } finally {
      setPromptImproving(false);
    }
  };
  const addFiles = (fileList: FileList | File[]) => {
    const nextFiles = Array.from(fileList).filter((file) => file.size > 0);
    if (nextFiles.length === 0) return;
    setAttachments((current) => {
      const slots = Math.max(0, MAX_ATTACHMENTS - current.length);
      return [
        ...current,
        ...nextFiles.slice(0, slots).map((file) => ({
          id: `${file.name}-${file.size}-${file.lastModified}-${crypto.randomUUID()}`,
          file
        }))
      ];
    });
  };
  const removeAttachment = (id: string) => {
    setAttachments((current) => current.filter((attachment) => attachment.id !== id));
  };
  const startBasicChatResize = (event: React.PointerEvent<HTMLDivElement>) => {
    if (viewMode !== 'overlay' || effectiveChatCollapsed) return;
    event.preventDefault();
    event.stopPropagation();
    event.currentTarget.setPointerCapture?.(event.pointerId);
    const startY = event.clientY;
    const startHeight = basicChatHeight;
    document.documentElement.classList.add('resizingChat');
    const onMove = (moveEvent: PointerEvent) => {
      moveEvent.preventDefault();
      updateBasicChatHeight(startHeight + startY - moveEvent.clientY);
    };
    const onUp = () => {
      document.documentElement.classList.remove('resizingChat');
      removeEventListener('pointermove', onMove);
      removeEventListener('pointerup', onUp);
      removeEventListener('pointercancel', onUp);
    };
    addEventListener('pointermove', onMove);
    addEventListener('pointerup', onUp);
    addEventListener('pointercancel', onUp);
  };
  const stopExternalLinkPropagation = (event: React.SyntheticEvent<HTMLAnchorElement>) => {
    event.stopPropagation();
  };
  const openExternalLink = (event: React.MouseEvent<HTMLAnchorElement>) => {
    event.preventDefault();
    event.stopPropagation();
    const href = event.currentTarget.href;
    if (!href) return;
    const opened = window.open(href, '_blank');
    if (opened) {
      opened.opener = null;
      return;
    }
    window.location.assign(href);
  };
  const startCollapsedChatDrag = (event: React.PointerEvent<HTMLButtonElement>) => {
    if (viewMode !== 'overlay' || !effectiveChatCollapsed || chatCollapsedForTutorial) return;
    event.preventDefault();
    event.currentTarget.setPointerCapture?.(event.pointerId);
    const startX = event.clientX;
    const startY = event.clientY;
    const startPosition = clampCollapsedChatPosition(collapsedChatPosition);
    let moved = false;
    document.documentElement.classList.add('draggingCollapsedChat');
    const onMove = (moveEvent: PointerEvent) => {
      moveEvent.preventDefault();
      const dx = moveEvent.clientX - startX;
      const dy = moveEvent.clientY - startY;
      if (Math.hypot(dx, dy) > 5) moved = true;
      setCollapsedChatPosition(clampCollapsedChatPosition({
        x: startPosition.x + dx,
        y: startPosition.y + dy
      }));
    };
    const finish = (cancelled = false) => {
      document.documentElement.classList.remove('draggingCollapsedChat');
      removeEventListener('pointermove', onMove);
      removeEventListener('pointerup', onUp);
      removeEventListener('pointercancel', onCancel);
      if (!moved && !cancelled) {
        setBasicChatCollapsed(false);
      }
    };
    const onUp = () => finish(false);
    const onCancel = () => finish(true);
    addEventListener('pointermove', onMove);
    addEventListener('pointerup', onUp);
    addEventListener('pointercancel', onCancel);
  };
  const handleCollapsedChatKeyDown = (event: React.KeyboardEvent<HTMLButtonElement>) => {
    if (event.key !== 'Enter' && event.key !== ' ') return;
    event.preventDefault();
    setBasicChatCollapsed(false);
  };
  const chatDragHandlers = {
    onDragEnter: (event: React.DragEvent) => {
      if (composerDisabled || !event.dataTransfer.types.includes('Files')) return;
      if (event.cancelable) event.preventDefault();
      dragDepthRef.current += 1;
      setDraggingFiles(true);
    },
    onDragOver: (event: React.DragEvent) => {
      if (composerDisabled || !event.dataTransfer.types.includes('Files')) return;
      event.preventDefault();
      event.dataTransfer.dropEffect = 'copy';
    },
    onDragLeave: (event: React.DragEvent) => {
      if (composerDisabled || !event.dataTransfer.types.includes('Files')) return;
      event.preventDefault();
      dragDepthRef.current = Math.max(0, dragDepthRef.current - 1);
      if (dragDepthRef.current === 0) setDraggingFiles(false);
    },
    onDrop: (event: React.DragEvent) => {
      if (composerDisabled || !event.dataTransfer.files.length) return;
      event.preventDefault();
      dragDepthRef.current = 0;
      setDraggingFiles(false);
      addFiles(event.dataTransfer.files);
    }
  };

  const modeToggle = (
    singleView ? null :
    <button
      className={`chromeIconButton splitToggle tooltip tooltipBottom ${viewMode === 'split' ? 'selected' : ''}`}
      onClick={() => setMode(viewMode === 'split' ? 'overlay' : 'split')}
      aria-label={viewMode === 'split' ? t('builder.view.useBasic') : t('builder.view.useSplit')}
      data-tip={viewMode === 'split' ? t('builder.view.useBasic') : t('builder.view.useSplit')}
    >
      <LayoutPanelLeft size={16} />
    </button>
  );
  const openProjectsPanel = () => {
    if (!signedIn) return;
    setShowProjects((open) => {
      const next = !open;
      if (next) void loadProjects().catch(() => undefined);
      return next;
    });
    setShowProfile(false);
    setShowHelp(false);
    setShowServices(false);
    if (location.pathname.startsWith('/profile')) nav('/');
  };
  const openProfilePanel = () => {
    if (!signedIn) return;
    setShowProfile(true);
    setShowProjects(false);
    setShowHelp(false);
    setShowServices(false);
    setBasicChatCollapsed(false);
    nav('/profile');
  };
  const closeProfilePanel = () => {
    setShowProfile(false);
    if (location.pathname.startsWith('/profile')) nav('/');
  };
  const openHelpPanel = () => {
    setShowHelp((open) => !open);
    setShowProjects(false);
    setShowProfile(false);
    setShowServices(false);
    setBasicChatCollapsed(false);
    if (location.pathname.startsWith('/profile')) nav('/');
  };
  const openServicesPanel = () => {
    if (!signedIn || !activeProject?.services || activeProject.services.length < 2) return;
    setShowServices((open) => !open);
    setShowProjects(false);
    setShowProfile(false);
    setShowHelp(false);
    setBasicChatCollapsed(false);
    if (location.pathname.startsWith('/profile')) nav('/');
  };
  const useOnboardingPrompt = (value: string) => {
    setPrompt(value);
    setShowProjects(false);
    setShowProfile(false);
    setShowHelp(false);
    setShowServices(false);
    setBasicChatCollapsed(false);
    requestAnimationFrame(() => textareaRef.current?.focus());
  };
  const dismissOnboarding = () => {
    setManualOnboardingOpen(false);
    if (!activeProject?.id) return;
    setDismissedOnboardingProjects((current) => {
      if (current[activeProject.id]) return current;
      const next = { ...current, [activeProject.id]: true };
      writeDismissedOnboardingProjects(next);
      return next;
    });
  };
  const openOnboardingTutorial = () => {
    if (!signedIn) return;
    setManualOnboardingOpen(true);
    setShowProjects(false);
    setShowProfile(false);
    setShowHelp(false);
    setShowServices(false);
    if (location.pathname.startsWith('/profile')) nav('/');
  };
  const runStudioNextAction = () => {
    if (agentActivityActive) {
      void interruptAgent();
      return;
    }
    if (projectArchived && activeProject) {
      requestProjectExport(activeProject);
      return;
    }
    if (noActiveProject) {
      setConfirmNewProject(true);
      return;
    }
    setBasicChatCollapsed(false);
    requestAnimationFrame(() => textareaRef.current?.focus());
  };
  const refreshBuilder = async () => {
    if (!signedIn) return;
    await Promise.all([
      loadProjects().catch(() => undefined),
      refreshQuota(),
      activeID
        ? api<Feed>(`/api/projects/${activeID}/feed`)
          .then((nextFeed) => setFeed((current) => mergeFeedSnapshot(current, nextFeed)))
          .catch(() => undefined)
        : Promise.resolve()
    ]);
  };
  const customPullRefreshEnabled = signedIn && !isAndroidBrowserPlatform();
  const pullRefreshTouchZoneEnabled = signedIn && (customPullRefreshEnabled || isAndroidBrowserPlatform());
  const pullRefresh = usePullToRefresh(customPullRefreshEnabled, refreshBuilder);
  const serviceSelectorButton = () => hasMultipleServices ? (
    <button
      className={`${showServices ? 'serviceSelectorButton selected' : 'serviceSelectorButton'} serviceSelectorInTitle`}
      onClick={openServicesPanel}
      disabled={!signedIn || busy}
      aria-label={t('service.selector', { name: selectedService?.name ?? selectedServiceLabel })}
      aria-expanded={showServices}
    >
      <span className="serviceSelectorDot" aria-hidden="true" />
      <span className="serviceSelectorName">{selectedServiceLabel}</span>
      <ChevronDown size={13} />
    </button>
  ) : null;
  const projectTitleButton = (slotClass: string, showIdleStop = false, showServiceSelector = false) => (
    <div className={`projectTitleControl ${slotClass}`}>
      <button className="projectTitleButton tooltip tooltipBottom" onClick={openProjectsPanel} disabled={!signedIn} aria-label={t('projects.title')} data-tip={signedIn ? t('builder.projects.tooltip') : t('auth.signInToCreateProjects')}>
        <span className="projectTitleText">
          <span className="projectTitleMain">{activeProject?.title ?? (signedIn ? t('builder.project.new') : t('auth.signInToBuild'))}</span>
          {projectChromeMeta && <span className="projectTitleSub">{projectChromeMeta}</span>}
        </span>
      </button>
      {showServiceSelector && serviceSelectorButton()}
      <span className="projectTitleMeta">
        <button className="projectTitleCount" type="button" onClick={openProjectsPanel} disabled={!signedIn} aria-label={t('projects.title')}>
          <FolderOpen size={15} />
          <span className={projectCapReached ? 'quotaFull' : ''}>{signedIn ? projectCapLabel : '-'}</span>
        </button>
        {showIdleStop && idleStopCountdown && (
          <span className="projectIdleStop tooltip" tabIndex={0} data-tip={idleStopTooltip} aria-label={idleStopTooltip}>{idleStopCountdown}</span>
        )}
        {showIdleStop && activeProject?.status === 'ready' && activePreviewURL && (
          <a
            className="projectExternalLink tooltip"
            href={activePreviewURL}
            target="_blank"
            rel="noopener noreferrer"
            data-no-pull-refresh="true"
            onTouchStart={stopExternalLinkPropagation}
            onPointerDown={stopExternalLinkPropagation}
            onMouseDown={stopExternalLinkPropagation}
            onClick={openExternalLink}
            aria-label={t('builder.preview.open')}
            data-tip={t('builder.preview.open')}
          >
            <ExternalLink size={15} />
          </a>
        )}
      </span>
    </div>
  );
  const builderChrome = (
    <div className="basicChatChrome">
      <button className="brand chatBrand tooltip tooltipBottom" onClick={() => nav('/')} aria-label={t('builder.brand.tooltip')} data-tip={t('builder.brand.tooltip')}>
        <StatusMark working={brandWorking} />
      </button>
      {projectTitleButton('chromeProjectTitle', false, true)}
      <nav className="chatNav">
        <div className="chromePill identityPill">
          {agentWorking && (
            <>
              <button className="chromeIconButton stopAgentButton tooltip tooltipBottom" onClick={interruptAgent} disabled={busy} aria-label={t('builder.stopAgent')} data-tip={t('builder.stopAgent')}><CircleStop size={16} /></button>
              <button
                className="chromeIconButton busyPolicyButton tooltip tooltipBottom"
                onClick={() => setBusyPolicy((policy) => policy === 'queue' ? 'steer' : 'queue')}
                aria-label={busyPolicy === 'queue' ? t('builder.busy.queue') : t('builder.busy.steer')}
                data-tip={busyPolicy === 'queue' ? t('builder.busy.queue') : t('builder.busy.steer')}
              >
                {busyPolicy === 'queue' ? 'Q' : 'S'}
              </button>
            </>
          )}
          <LanguageToggle className="chromeIconButton tooltip tooltipBottom languageChromeButton" />
          <button className={showProfile ? 'chromeIconButton selected tooltip tooltipBottom' : 'chromeIconButton tooltip tooltipBottom'} onClick={showProfile ? closeProfilePanel : openProfilePanel} disabled={!signedIn} aria-label={t('nav.profile')} data-tip={signedIn ? t('builder.profile.tooltip') : t('auth.signInToOpenProfile')}><UserRound size={16} /></button>
          {me.isAdmin && <button className="chromeIconButton tooltip tooltipBottom" onClick={() => nav('/admin')} aria-label={t('nav.admin')} data-tip={t('builder.admin.tooltip')}><Settings size={16} /></button>}
        </div>
        <div className="chromePill actionChromePill">
          {!me.user && (
            <>
              {useDevAuthPrimary ? (
                <button className="chromeAuthLink chromeAuthButton" onClick={() => void startDevAuth(me)}>{t('auth.signIn')}</button>
              ) : (
                <>
                  <a className={`chromeAuthLink ${!googleReady ? 'disabled' : ''}`} href={googleAuthHref}>{t('auth.signIn')}</a>
                  {me.auth?.devAuth && <button className="chromeAuthLink chromeAuthButton" onClick={() => void startDevAuth(me)}>{t('auth.dev')}</button>}
                </>
              )}
            </>
          )}
          <button
            className={showHelp ? 'chromeIconButton selected tooltip tooltipBottom' : 'chromeIconButton tooltip tooltipBottom'}
            onClick={openHelpPanel}
            aria-label={t('help.tooltip')}
            aria-expanded={showHelp}
            data-tip={t('help.tooltip')}
          >
            <CircleHelp size={16} />
          </button>
          {modeToggle}
          {activeProject?.status === 'ready' && activePreviewURL && (
            <a
              className="chromeIconButton desktopPreviewExternalLink tooltip tooltipBottom"
              href={activePreviewURL}
              target="_blank"
              rel="noopener noreferrer"
              data-no-pull-refresh="true"
              onTouchStart={stopExternalLinkPropagation}
              onPointerDown={stopExternalLinkPropagation}
              onMouseDown={stopExternalLinkPropagation}
              onClick={openExternalLink}
              aria-label={t('builder.preview.open')}
              data-tip={t('builder.preview.open')}
            >
              <ExternalLink size={16} />
            </a>
          )}
          {viewMode === 'overlay' && <button className="chromeIconButton collapseChatButton tooltip tooltipBottom" onClick={() => setBasicChatCollapsed(true)} aria-label={t('builder.chat.collapse')} data-tip={t('builder.chat.collapse')}><Minimize2 size={16} /></button>}
        </div>
      </nav>
    </div>
  );

  const chat = (
    <section className={`chatPane ${draggingFiles ? 'dragActive' : ''} ${utilityScreenOpen ? 'screenOpen' : ''} ${agentActivityActive ? 'agentActive' : ''} ${prompt.trim() ? 'hasDraft' : ''}`} {...chatDragHandlers}>
      <a className="poweredBy" href="https://fibe.gg" target="_blank" rel="noopener noreferrer">
        {t('builder.poweredBy')} <span>fibe.gg</span>
      </a>
      {projectTitleButton('chatProjectTitle', true, true)}
      {builderChrome}
      {showProjects && <ProjectList projects={currentProjects} activeID={activeID} projectCap={projectCap} busy={busy} exportingID={exportingID} controllingID={controllingProjectID} onSelect={(id) => { setActiveID(id); setShowProjects(false); }} onNew={() => setConfirmNewProject(true)} onRename={renameProject} onDelete={setDeleteTarget} onExport={requestProjectExport} onControlPlayground={controlProjectPlayground} onClose={() => setShowProjects(false)} />}
      {showProfile && <ProfilePanel me={me} onClose={closeProfilePanel} onOpenTutorial={openOnboardingTutorial} />}
      {showHelp && <HelpPanel markdown={t('help.markdown')} onClose={() => setShowHelp(false)} />}
      {showServices && activeProject?.services && <ServicePanel services={activeProject.services} selectedName={selectedService?.name} busy={busy} onSelect={(service) => void selectService(service)} onClose={() => setShowServices(false)} />}
      {!utilityScreenOpen && (
        <>
          <div className={`studioNowStrip ${studioState}`}>
            <div className="studioNowStatus">
              <span className="studioStatusDot" />
              <strong>{studioNowMeta}</strong>
              {idleStopCountdown && !agentActivityActive && <span>{t('builder.idleStop.label', { time: idleStopCountdown })}</span>}
            </div>
            <span className="studioNextEyebrow">{t('builder.nextAction.eyebrow')}</span>
            <button className={agentActivityActive ? 'studioNextAction danger' : 'studioNextAction'} type="button" onClick={runStudioNextAction}>
              <StudioNextIcon size={13} />
              <span>{studioNextActionLabel}</span>
            </button>
          </div>
          <div className="messages" ref={messagesRef}>
            {rows.map((row) => row.kind === 'notification'
              ? <AgentNotificationRow key={row.id} body={row.body} active={row.active} elapsedMs={row.elapsedMs} elapsedStartedAt={row.elapsedStartedAt} />
              : <UserMessageRow key={row.id} row={row} />
            )}
            {agentDelayStatus && !hasActiveNotification && <AgentNotificationRow body={agentDelayLabel} active={agentDelayStatus.active} tone={agentDelayStatus.tone} />}
            {agentWorking && !agentDelayStatus && !hasActiveNotification && <AgentNotificationRow body={agentWorkingLabel} active />}
          </div>
          {draggingFiles && <div className="dropOverlay"><Paperclip size={24} /> {t('builder.dropFiles')}</div>}
          <div className={`composer ${attachments.length > 0 ? 'hasAttachments' : ''}`}>
            <input ref={fileInputRef} type="file" multiple hidden onChange={(event) => {
              if (event.currentTarget.files) addFiles(event.currentTarget.files);
              event.currentTarget.value = '';
            }} />
            {attachments.length > 0 && (
              <div className="attachmentTray">
                <span className="attachmentTrayCount" aria-label={`${attachments.length}/${MAX_ATTACHMENTS}`}>
                  {attachments.length}/{MAX_ATTACHMENTS}
                </span>
                {attachments.map((attachment) => (
                  <span className="attachmentChip" key={attachment.id}>
                    <Paperclip size={14} />
                    <span>{attachment.file.name}</span>
                    <button onClick={() => removeAttachment(attachment.id)} aria-label={t('builder.removeAttachment', { name: attachment.file.name })}><X size={13} /></button>
                  </span>
                ))}
              </div>
            )}
            <button className="attachButton" type="button" onClick={() => fileInputRef.current?.click()} disabled={composerDisabled || attachments.length >= MAX_ATTACHMENTS} aria-label={t('builder.attachFiles')}>
              <Paperclip size={22} />
            </button>
            <div className="composerTextSlot">
              <textarea ref={textareaRef} value={prompt} onChange={(e) => setPrompt(e.target.value)} onKeyDown={handleComposerKeyDown} placeholder={inputPlaceholder} rows={1} disabled={composerDisabled} />
              {hourQuota && (
                <span className="composerQuotaBadge tooltip" tabIndex={0} data-tip={hourQuotaTooltip} aria-label={`${t('builder.hours.left')}: ${hourQuotaLabel}`}>
                  {hourQuotaLabel}
                </span>
              )}
            </div>
            <button className={`sendButton ${messageSubmitting ? 'working' : ''}`} disabled={!canSend} onClick={createOrSend}>
              {messageSubmitting ? <Loader2 className="spinIcon" size={22} /> : <Send size={22} />}
            </button>
            <div className="composerPromptTools">
              <button type="button" onClick={() => void improveCurrentPrompt()} disabled={composerDisabled || promptImproving} aria-label={t('builder.promptTools.improve')} title={t('builder.promptTools.improve')}>{promptImproving ? <Loader2 size={13} className="spinIcon" /> : <Sparkles size={13} />} <span className="composerToolLabel">{t('builder.promptTools.improve')}</span></button>
              <button type="button" onClick={() => fileInputRef.current?.click()} disabled={composerDisabled || attachments.length >= MAX_ATTACHMENTS} aria-label={t('builder.promptTools.reference')} title={t('builder.promptTools.reference')}><ImagePlus size={13} /> <span className="composerToolLabel">{t('builder.promptTools.reference')}</span></button>
              <button type="button" onClick={openOnboardingTutorial} disabled={!signedIn} aria-label={t('builder.promptTools.starters')} title={t('builder.promptTools.starters')}><BookOpen size={13} /> <span className="composerToolLabel">{t('builder.promptTools.starters')}</span></button>
              <span className={`composerModeHint ${agentActivityActive ? 'active' : composerHintTone}`} aria-live="polite">{composerModeHint}</span>
            </div>
          </div>
        </>
      )}
    </section>
  );
  const minimizedChatBar = (
    <button
      className={`minimizedChatBar ${brandWorking ? 'working' : ''}`}
      aria-label={t('builder.expandChat')}
      onPointerDown={startCollapsedChatDrag}
      onKeyDown={handleCollapsedChatKeyDown}
      {...chatDragHandlers}
    >
      <StatusMark working={brandWorking} />
    </button>
  );

  const previewTitle = activeProject?.status === 'launching' ? t('builder.preview.startingTitle') : t('builder.preview.preparingTitle');
  const previewBody = activeProject?.status === 'launching'
    ? t('builder.preview.launchingBody')
    : t('builder.preview.preparingBody');
  const launchFailedBody = activeProject?.errorMessage?.trim() || t('builder.preview.launchFailedBody');
  const connectingCanvasBody = previewReady
    ? t('builder.preview.respondedBody')
    : t('builder.preview.warmingBody');
  const previewContent = (
    <>
      {projectsLoading ? (
        <CanvasLoader title={t('empty.loadingProjectsTitle')} body={t('empty.loadingProjectsBody')} />
      ) : noActiveProject ? (
        <EmptyCanvas title={t('empty.noProjectTitle')} body={t('empty.noProjectBody')} />
      ) : projectArchived ? (
        <CanvasLoader title={t('builder.preview.archivedTitle')} body={t('builder.preview.archivedBody')} />
      ) : showOnboardingWizard ? (
        <>
          {activePreviewURL && previewDisplayable && (
            <iframe
              title={t('builder.preview.frameTitle')}
              src={activePreviewURL}
              className={iframeLoaded ? 'loaded' : ''}
              onLoad={() => {
                if (previewDisplayable) setIframeLoaded(true);
              }}
            />
          )}
          <OnboardingGallery ready={onboardingReady} onUsePrompt={useOnboardingPrompt} onStart={dismissOnboarding} onDismiss={dismissOnboarding} />
        </>
      ) : activeProject?.status === 'error' && agentActivityActive ? (
        <CanvasLoader title={agentWorkingLabel} body={t('builder.preview.agentWorkingBody')} />
      ) : activeProject?.status === 'error' && !previewMaintenance ? (
        <CanvasLoader title={t('builder.preview.launchFailedTitle')} body={launchFailedBody} tone="error" />
      ) : activeProject?.status === 'stopped' ? (
        <CanvasLoader title={t('builder.preview.stoppedTitle')} body={t('builder.preview.stoppedBody')} />
      ) : activePreviewURL && previewDisplayable ? (
        <>
          <iframe
            title={t('builder.preview.frameTitle')}
            src={activePreviewURL}
            className={previewDisplayable && iframeLoaded ? 'loaded' : ''}
            onLoad={() => {
              if (previewDisplayable) setIframeLoaded(true);
            }}
          />
          {(!previewDisplayable || !iframeLoaded) && <CanvasLoader title={t('builder.preview.connectingTitle')} body={connectingCanvasBody} />}
        </>
      ) : isProjectStarting ? (
        <CanvasLoader title={previewTitle} body={previewBody} />
      ) : activeProject?.status === 'ready' && activePreviewURL ? (
        <>
          <iframe
            title={t('builder.preview.frameTitle')}
            src="about:blank"
            className=""
            onLoad={() => undefined}
          />
          <CanvasLoader title={t('builder.preview.connectingTitle')} body={connectingCanvasBody} />
        </>
      ) : activeProject ? (
        <CanvasLoader title={previewTitle} body={previewBody} />
      ) : (
        <EmptyCanvas title={t('empty.noProjectTitle')} body={t('empty.noProjectBody')} />
      )}
      {viewMode === 'split' && <div className={`canvasStatus ${brandWorking ? 'working' : ''}`}><span /> {canvasStatusLabel}</div>}
    </>
  );
  const preview = (
    <section
      className={`previewPane ${viewMode === 'overlay' && !effectiveChatCollapsed ? 'chatExpanded' : ''}`}
      style={viewMode === 'overlay' && !effectiveChatCollapsed ? ({ '--basic-chat-height': `${basicChatHeight}px` } as React.CSSProperties) : undefined}
    >
      <div className="previewTopChrome">{builderChrome}</div>
      <div className="previewContent">
        {pullRefreshTouchZoneEnabled && <div className="pullRefreshTouchZone" aria-hidden="true" />}
        {previewContent}
      </div>
      {viewMode === 'overlay' && (
        <div
          className={`overlayChat ${utilityScreenOpen ? 'utilityOpen' : ''} ${effectiveChatCollapsed ? 'collapsed minimized' : ''}`}
          style={effectiveChatCollapsed
            ? collapsedChatStyle(collapsedChatPosition)
            : ({ '--basic-chat-height': `${basicChatHeight}px` } as React.CSSProperties)}
        >
          {!effectiveChatCollapsed && <div className="chatResizeHandle" data-no-pull-refresh="true" aria-label={t('builder.resizeChat')} onPointerDown={startBasicChatResize} />}
          {effectiveChatCollapsed ? minimizedChatBar : chat}
        </div>
      )}
      {confirmNewProject && <ConfirmNewProject projectCap={projectCap} projectCount={quotaProjectCount} busy={busy} onCancel={() => setConfirmNewProject(false)} onConfirm={createProject} />}
      {deleteTarget && <ConfirmDeleteProject project={deleteTarget} busy={busy} onCancel={() => setDeleteTarget(null)} onConfirm={deleteProject} />}
      {exportTarget && <ConfirmExportProject project={exportTarget} busy={busy || exportingID === exportTarget.id} busyMode={exportingMode} githubConnected={githubConnected} githubNeedsReconnect={githubNeedsReconnect} onCancel={() => setExportTarget(null)} onGithub={(repoName, privateRepo) => void exportProject(exportTarget, repoName, privateRepo)} onZip={() => void exportProjectZip(exportTarget)} onConnectGithub={connectGithub} />}
      {dialog && <AppDialog dialog={dialog} onClose={() => setDialog(null)} />}
    </section>
  );

  return (
    <div className={`${viewMode === 'split' ? 'builder split' : 'builder overlay'} ${showOnboardingWizard ? 'onboardingActive' : ''}`} data-mode={modeLabel}>
      <div
        className={`pullRefreshIndicator ${pullRefresh.visible ? 'visible' : ''} ${pullRefresh.ready ? 'ready' : ''}`}
        style={{ '--pull-distance': `${pullRefresh.distance}px` } as React.CSSProperties}
        aria-hidden={!pullRefresh.visible}
        aria-live="polite"
      >
        <span>{pullRefresh.refreshing ? <Loader2 size={15} className="spin" /> : <ChevronDown size={16} />}</span>
        <strong>{pullRefresh.refreshing ? t('pullRefresh.refreshing') : pullRefresh.ready ? t('pullRefresh.release') : t('pullRefresh.pull')}</strong>
      </div>
      {viewMode === 'split' && chat}
      {preview}
    </div>
  );
}

function selectedProjectService(project?: Project): ProjectService | undefined {
  if (!project?.services?.length) return undefined;
  const selected = project.selectedServiceName;
  return project.services.find((service) => service.name === selected)
    ?? project.services.find((service) => service.name === 'app')
    ?? project.services[0];
}

function projectPageTitle(title: string, serviceName: string | undefined, t: (key: TranslationKey, params?: Record<string, string | number>) => string): string {
  const normalizedServiceName = (serviceName ?? '').trim();
  if (!normalizedServiceName) return title;
  return t('page.projectServiceTitle', { title, service: normalizedServiceName });
}

function improvePromptDraft(rawPrompt: string, projectTitle: string | undefined, t: (key: TranslationKey, params?: Record<string, string | number>) => string): string {
  const draft = rawPrompt.trim().replace(/\s+/g, ' ');
  const title = projectTitle?.trim();
  const appContext = title ? t('promptImprove.fallback.namedApp', { title }) : t('promptImprove.fallback.currentApp');
  if (!draft) {
    return t('promptImprove.fallback.empty', { app: appContext });
  }
  const normalizedDraft = draft.toLowerCase();
  if (normalizedDraft.includes('do not replace it with an unrelated app') || normalizedDraft.includes('не замінюй') || normalizedDraft.includes('не заменяй')) {
    return draft;
  }
  return [
    t('promptImprove.fallback.intro', { app: appContext }),
    t('promptImprove.fallback.requestedChange', { draft }),
    t('promptImprove.fallback.preserveDomain'),
    t('promptImprove.fallback.polish'),
    t('promptImprove.fallback.verify')
  ].join('\n');
}

type CollapsedChatPosition = { x: number; y: number };

function initialCollapsedChatPosition(): CollapsedChatPosition {
  const stored = localStorage.getItem(COLLAPSED_CHAT_POSITION_KEY);
  if (stored) {
    try {
      const parsed = JSON.parse(stored) as Partial<CollapsedChatPosition>;
      if (typeof parsed.x === 'number' && typeof parsed.y === 'number') {
        return clampCollapsedChatPosition({ x: parsed.x, y: parsed.y });
      }
    } catch {
      localStorage.removeItem(COLLAPSED_CHAT_POSITION_KEY);
    }
  }
  return defaultCollapsedChatPosition();
}

function defaultCollapsedChatPosition(): CollapsedChatPosition {
  return clampCollapsedChatPosition({
    x: Math.round(currentViewportWidth() / 2),
    y: Math.round(currentViewportHeight() - 48)
  });
}

function clampCollapsedChatPosition(position: CollapsedChatPosition): CollapsedChatPosition {
  const minX = COLLAPSED_CHAT_EDGE_MARGIN;
  const minY = COLLAPSED_CHAT_EDGE_MARGIN;
  const maxX = Math.max(minX, currentViewportWidth() - COLLAPSED_CHAT_EDGE_MARGIN);
  const maxY = Math.max(minY, currentViewportHeight() - COLLAPSED_CHAT_EDGE_MARGIN);
  return {
    x: Math.min(maxX, Math.max(minX, Math.round(position.x))),
    y: Math.min(maxY, Math.max(minY, Math.round(position.y)))
  };
}

function collapsedChatStyle(position: CollapsedChatPosition): React.CSSProperties {
  const clamped = clampCollapsedChatPosition(position);
  return {
    left: `${clamped.x}px`,
    top: `${clamped.y}px`,
    right: 'auto',
    bottom: 'auto',
    transform: 'translate(-50%, -50%)'
  };
}

function mergeFeedSnapshot(current: Feed | null, next: Feed): Feed {
  if (!current || current.project.id !== next.project.id) return next;
  if (next.live?.isProcessing || next.live?.streamText) return next;
  if (next.project.status !== 'ready') return next;
  const currentLive = current.live;
  if (!currentLive?.isProcessing || !currentLive.streamText) return next;
  if (feedHasAssistantAfterLatestUser(next)) return next;
  const nextLiveIdle = feedLiveIdle(next);

  return {
    ...next,
    messages: next.messages?.length ? next.messages : current.messages,
    activity: next.activity?.length ? next.activity : current.activity,
    live: {
      ...currentLive,
      conversationId: next.live?.conversationId ?? currentLive.conversationId,
      isProcessing: !nextLiveIdle,
      queuedTurns: typeof next.live?.queuedTurns === 'number' ? next.live.queuedTurns : currentLive.queuedTurns,
      startedAt: currentLive.startedAt ?? next.live?.startedAt
    }
  };
}

function feedWithPendingMessages(feed: Feed | null, project: Project | undefined, activeID: string, pending: Message[]): Feed | null {
  if (!activeID) return feed;
  const base = feed?.project.id === activeID
    ? feed
    : project
      ? { project, localMessages: [], messages: [], activity: [], live: null }
      : null;
  if (!base || pending.length === 0) return base;
  const localMessages = [...(base.localMessages ?? [])];
  const seen = new Set(localMessages.map((message) => message.id));
  for (const message of pending) {
    if (!seen.has(message.id) && !localMessages.some((serverMessage) => messageMatchesServerCopy(message, serverMessage))) {
      localMessages.push(message);
      seen.add(message.id);
    }
  }
  return { ...base, localMessages };
}

function messageMatchesServerCopy(pending: Message, serverMessage: Message): boolean {
  if (pending.id === serverMessage.id) return true;
  if (pending.role !== 'user' || serverMessage.role !== 'user') return false;
  if (pending.body !== serverMessage.body) return false;
  if (!attachmentsMatch(pending.attachments ?? [], serverMessage.attachments ?? [])) return false;
  const pendingTime = Date.parse(pending.createdAt);
  const serverTime = Date.parse(serverMessage.createdAt);
  if (Number.isNaN(pendingTime) || Number.isNaN(serverTime)) return false;
  return Math.abs(pendingTime - serverTime) <= PENDING_MESSAGE_RECONCILE_MS;
}

function attachmentsMatch(left: Message['attachments'], right: Message['attachments']): boolean {
  const leftAttachments = left ?? [];
  const rightAttachments = right ?? [];
  if (leftAttachments.length !== rightAttachments.length) return false;
  return leftAttachments.every((attachment, index) => {
    const candidate = rightAttachments[index];
    return Boolean(candidate)
      && attachment.filename === candidate.filename
      && (attachment.contentType ?? '') === (candidate.contentType ?? '')
      && (attachment.size ?? 0) === (candidate.size ?? 0);
  });
}

function addPendingMessage(current: Record<string, Message[]>, projectID: string, message: Message): Record<string, Message[]> {
  return { ...current, [projectID]: [...(current[projectID] ?? []), message] };
}

function replacePendingMessage(current: Record<string, Message[]>, projectID: string, pendingID: string, message: Message): Record<string, Message[]> {
  const pending = current[projectID] ?? [];
  if (pending.length === 0) return current;
  return { ...current, [projectID]: pending.map((candidate) => candidate.id === pendingID ? message : candidate) };
}

function removePendingMessage(current: Record<string, Message[]>, projectID: string, messageID: string): Record<string, Message[]> {
  const pending = current[projectID] ?? [];
  if (pending.length === 0) return current;
  const nextPending = pending.filter((message) => message.id !== messageID);
  if (nextPending.length === pending.length) return current;
  if (nextPending.length === 0) {
    const { [projectID]: _removed, ...rest } = current;
    return rest;
  }
  return { ...current, [projectID]: nextPending };
}

function normalizeActiveNotificationRows(rows: FeedRow[]): FeedRow[] {
  let latestNotificationIndex = -1;
  let latestActiveIndex = -1;
  rows.forEach((row, index) => {
    if (row.kind !== 'notification') return;
    latestNotificationIndex = index;
    if (row.active) latestActiveIndex = index;
  });
  if (latestActiveIndex === -1) return rows;

  return rows.map((row, index) => {
    if (row.kind !== 'notification' || !row.active) return row;
    if (latestActiveIndex === latestNotificationIndex && index === latestActiveIndex) return row;
    return { ...row, active: false };
  });
}

type PullRefreshState = {
  distance: number;
  ready: boolean;
  refreshing: boolean;
  visible: boolean;
};

function usePullToRefresh(enabled: boolean, onRefresh: () => Promise<void> | void): PullRefreshState {
  const [state, setState] = useState<PullRefreshState>({ distance: 0, ready: false, refreshing: false, visible: false });
  const refreshRef = useRef(onRefresh);
  const gestureRef = useRef({ active: false, startY: 0, distance: 0, refreshing: false });
  const resetTimerRef = useRef<number | null>(null);

  useEffect(() => {
    refreshRef.current = onRefresh;
  }, [onRefresh]);

  useEffect(() => {
    const clearResetTimer = () => {
      if (resetTimerRef.current == null) return;
      window.clearTimeout(resetTimerRef.current);
      resetTimerRef.current = null;
    };
    const clearRefreshState = () => {
      clearResetTimer();
      gestureRef.current.refreshing = false;
      gestureRef.current.distance = 0;
      setState({ distance: 0, ready: false, refreshing: false, visible: false });
    };

    if (!enabled) {
      gestureRef.current = { active: false, startY: 0, distance: 0, refreshing: false };
      clearRefreshState();
      return;
    }

    const updateDistance = (distance: number, refreshing = gestureRef.current.refreshing) => {
      gestureRef.current.distance = distance;
      setState({
        distance,
        refreshing,
        ready: distance >= PULL_REFRESH_TRIGGER,
        visible: refreshing || distance > 0
      });
    };

    const reset = () => {
      gestureRef.current.active = false;
      gestureRef.current.distance = 0;
      if (!gestureRef.current.refreshing) {
        setState({ distance: 0, ready: false, refreshing: false, visible: false });
      }
    };

    const start = (event: TouchEvent) => {
      if (gestureRef.current.refreshing || event.touches.length !== 1 || !canPullRefreshFrom(event.target)) return;
      gestureRef.current.active = true;
      gestureRef.current.startY = event.touches[0].clientY;
      gestureRef.current.distance = 0;
    };

    const move = (event: TouchEvent) => {
      if (!gestureRef.current.active) return;
      if (event.touches.length !== 1) {
        reset();
        return;
      }
      const delta = event.touches[0].clientY - gestureRef.current.startY;
      if (delta <= 0) {
        reset();
        return;
      }
      if (!canPullRefreshFrom(event.target)) {
        reset();
        return;
      }
      if (event.cancelable) event.preventDefault();
      updateDistance(Math.min(PULL_REFRESH_MAX, Math.pow(delta, 0.88)));
    };

    const finish = () => {
      if (!gestureRef.current.active) return;
      const shouldRefresh = gestureRef.current.distance >= PULL_REFRESH_TRIGGER;
      gestureRef.current.active = false;
      if (!shouldRefresh) {
        reset();
        return;
      }
      gestureRef.current.refreshing = true;
      setState({ distance: PULL_REFRESH_TRIGGER, ready: true, refreshing: true, visible: true });
      const complete = () => {
        if (!gestureRef.current.refreshing) return;
        clearResetTimer();
        resetTimerRef.current = window.setTimeout(clearRefreshState, PULL_REFRESH_SETTLE_DELAY_MS);
      };
      clearResetTimer();
      resetTimerRef.current = window.setTimeout(clearRefreshState, PULL_REFRESH_TIMEOUT_MS);
      Promise.resolve()
        .then(() => refreshRef.current())
        .catch(() => undefined)
        .finally(complete);
    };

    window.addEventListener('touchstart', start, { passive: true });
    window.addEventListener('touchmove', move, { passive: false });
    window.addEventListener('touchend', finish, { passive: true });
    window.addEventListener('touchcancel', reset, { passive: true });
    window.addEventListener('blur', reset);
    window.addEventListener('pagehide', clearRefreshState);
    document.addEventListener('visibilitychange', clearRefreshState);
    return () => {
      window.removeEventListener('touchstart', start);
      window.removeEventListener('touchmove', move);
      window.removeEventListener('touchend', finish);
      window.removeEventListener('touchcancel', reset);
      window.removeEventListener('blur', reset);
      window.removeEventListener('pagehide', clearRefreshState);
      document.removeEventListener('visibilitychange', clearRefreshState);
      clearResetTimer();
    };
  }, [enabled]);

  return state;
}

function canPullRefreshFrom(target: EventTarget | null) {
  if (!(target instanceof HTMLElement)) return false;
  if (target.closest('a, button, textarea, input, select, summary, [contenteditable="true"], [role="button"], [role="menuitem"], [role="radio"], [role="dialog"], [data-no-pull-refresh="true"]')) return false;
  if (window.scrollY > 1) return false;

  let node: HTMLElement | null = target;
  while (node && node !== document.body && node !== document.documentElement) {
    const style = window.getComputedStyle(node);
    const scrollable = /(auto|scroll|overlay)/.test(style.overflowY) && node.scrollHeight > node.clientHeight + 1;
    if (scrollable && node.scrollTop > 1) return false;
    node = node.parentElement;
  }
  return true;
}

function readDismissedOnboardingProjects(): Record<string, boolean> {
  try {
    const parsed = JSON.parse(localStorage.getItem(ONBOARDING_DISMISSED_KEY) ?? '{}') as unknown;
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return {};
    return Object.fromEntries(Object.entries(parsed).filter(([, value]) => value === true));
  } catch {
    return {};
  }
}

function writeDismissedOnboardingProjects(value: Record<string, boolean>) {
  localStorage.setItem(ONBOARDING_DISMISSED_KEY, JSON.stringify(value));
}

function visibleShellNotices(notices: UserNotice[], now = Date.now()): UserNotice[] {
  return notices.filter((notice) => !isStaleProjectDeletionNotice(notice, now) && !isProjectQuotaNotice(notice));
}

function isStaleProjectDeletionNotice(notice: UserNotice, now: number): boolean {
  if (!notice.body.startsWith('Project deletion started:')) return false;
  const created = Date.parse(notice.createdAt);
  return !Number.isNaN(created) && now - created > STALE_PROJECT_DELETION_NOTICE_MS;
}

function isProjectQuotaNotice(notice: UserNotice): boolean {
  return notice.sender === 'system' && notice.body.startsWith('Project quota:');
}

createRoot(document.getElementById('root')!).render(
  <I18nProvider>
    <App />
  </I18nProvider>
);
