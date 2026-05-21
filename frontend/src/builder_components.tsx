import { useEffect, useState, type MouseEvent, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { Check, ChevronLeft, ChevronRight, CircleAlert, CircleHelp, Download, FileOutput, GitBranch, Loader2, MessageSquare, Minimize2, MoreHorizontal, Paperclip, Pencil, Play, Plus, RotateCcw, Sparkles, Square, Trash2, X } from 'lucide-react';
import type { AppDialogConfig, MessageAttachment, Project, ProjectService, UserFeedRow } from './domain';
import { formatElapsedDuration, formatMessageTime } from './format';
import { elapsedDurationLabels, statusLabel, useI18n } from './i18n';

export function ProjectList({ projects, activeID, projectCap, busy, exportingID, controllingID, onSelect, onNew, onRename, onDelete, onExport, onControlPlayground, onClose }: { projects: Project[]; activeID: string; projectCap: number | null; busy: boolean; exportingID: string; controllingID: string; onSelect: (id: string) => void; onNew: () => void; onRename: (project: Project, title: string) => Promise<void>; onDelete: (project: Project) => void; onExport: (project: Project) => void; onControlPlayground: (project: Project, action: 'start' | 'stop' | 'restart') => Promise<void>; onClose: () => void }) {
  const { locale, t } = useI18n();
  const [editingID, setEditingID] = useState('');
  const [draftTitle, setDraftTitle] = useState('');
  const [menuID, setMenuID] = useState('');
  const quotaProjectCount = projects.filter((project) => project.status !== 'archived' && project.status !== 'deleting').length;
  const capReached = projectCap != null && quotaProjectCount >= projectCap;
  const projectCountLabel = projectCap == null
    ? t(quotaProjectCount === 1 ? 'projects.count.one' : 'projects.count.many', { count: quotaProjectCount })
    : t('projects.count.cap', { count: quotaProjectCount, cap: projectCap });
  const startEdit = (project: Project) => {
    setEditingID(project.id);
    setDraftTitle(project.title);
  };
  const cancelEdit = () => {
    setEditingID('');
    setDraftTitle('');
  };
  const saveTitle = async (project: Project) => {
    const title = draftTitle.trim();
    if (!title || title === project.title) {
      cancelEdit();
      return;
    }
    await onRename(project, title);
    cancelEdit();
  };
  const runPlaygroundAction = async (project: Project, action: 'start' | 'stop' | 'restart') => {
    setMenuID('');
    await onControlPlayground(project, action);
  };
  const canStartPlayground = (project: Project) => project.status === 'stopped';
  const canStopPlayground = (project: Project) => project.status === 'ready';
  const canRestartPlayground = (project: Project) => project.status === 'ready';
  const projectRuntimeState = (project: Project) => {
    if (controllingID === project.id) return t('projects.actions.working');
    switch (project.status) {
      case 'ready':
        return t('projects.actions.ready');
      case 'stopped':
        return t('projects.actions.paused');
      case 'creating':
      case 'launching':
        return t('projects.actions.starting');
      case 'error':
        return t('projects.actions.error');
      case 'archived':
        return t('projects.actions.archived');
      default:
        return statusLabel(project.status, t);
    }
  };
  const projectRowMeta = (project: Project) => {
    const serviceCount = project.services?.length ?? 0;
    const updatedTime = formatMessageTime(project.updatedAt, locale);
    return updatedTime
      ? t('projects.rowMeta.updated', { services: serviceCount, time: updatedTime })
      : t('projects.rowMeta.servicesOnly', { services: serviceCount });
  };
  const projectRowDetail = (project: Project) => {
    switch (project.status) {
      case 'creating':
      case 'launching':
        return t('projects.rowDetail.starting');
      case 'error':
        return project.errorMessage?.trim() || t('projects.rowDetail.error');
      case 'stopped':
        return t('projects.rowDetail.stopped');
      case 'archived':
        return t('projects.rowDetail.archived');
      default:
        return '';
    }
  };
  return (
    <div className="projectList">
      <div className="inlinePanelHeader projectPanelHeader">
        <div>
          <span className="eyebrow">{t('projects.title')}</span>
          <strong>{projectCountLabel}</strong>
          {capReached && <small className="projectPanelLimit"><CircleAlert size={12} /> {t('projects.quotaFull')}</small>}
        </div>
        <button className="projectDelete" onClick={onClose} aria-label={t('projects.close')}><X size={15} /></button>
      </div>
      <div className={menuID ? 'projectRows menuOpen' : 'projectRows'}>
        {projects.map((project, index) => {
          const detail = projectRowDetail(project);
          const menuOpensUp = index > 0;
          return (
          <div key={project.id} className={`projectRow ${project.id === activeID ? 'selected' : ''} ${menuID === project.id ? 'menuActive' : ''}`}>
            {editingID === project.id ? (
              <form className="projectTitleEdit" onSubmit={(event) => { event.preventDefault(); void saveTitle(project); }}>
                <input
                  className="projectEditInput"
                  value={draftTitle}
                  autoFocus
                  maxLength={80}
                  onChange={(event) => setDraftTitle(event.target.value)}
                  onKeyDown={(event) => {
                    if (event.key === 'Escape') {
                      event.preventDefault();
                      cancelEdit();
                    }
                  }}
                  aria-label={t('builder.project.name')}
                />
                <button className="projectRowIcon" type="submit" disabled={busy || !draftTitle.trim()} aria-label={t('projects.saveName')}><Check size={15} /></button>
                <button className="projectRowIcon" type="button" onClick={cancelEdit} aria-label={t('projects.cancelRename')}><X size={15} /></button>
              </form>
            ) : (
              <>
                <span className={`projectRowMark ${project.status}`} aria-hidden="true">
                  {(project.title.trim()[0] || 'L').toUpperCase()}
                </span>
                <button className="projectSelect" onClick={() => onSelect(project.id)}>
                  <span className="projectSelectTitleLine">
                    <span>{project.title}</span>
                    {project.id === activeID && <strong className="projectCurrentBadge">{t('service.current')}</strong>}
                  </span>
                  <small className="projectSelectMeta">{projectRowMeta(project)}</small>
                  {detail && <small className={`projectSelectDetail ${project.status}`}>{detail}</small>}
                </button>
                <em className={`projectStatusChip ${project.status}`}>{statusLabel(project.status, t)}</em>
                <div className="projectRowActions">
                  <button
                    className="projectRowIcon"
                    disabled={busy || exportingID === project.id || project.status === 'deleting'}
                    onClick={() => onExport(project)}
                    aria-label={t('projects.export.aria', { title: project.title })}
                    title={t('projects.export.title')}
                  >
                    {exportingID === project.id ? <Loader2 className="spinIcon" size={14} /> : <FileOutput size={14} />}
                  </button>
                  <button className="projectRowIcon" onClick={() => startEdit(project)} aria-label={t('projects.rename.aria', { title: project.title })}><Pencil size={14} /></button>
                  <button className="projectDelete" onClick={() => onDelete(project)} aria-label={t('projects.delete.aria', { title: project.title })}><Trash2 size={15} /></button>
                  <div className="projectMenuAnchor">
                    <button
                      className="projectRowIcon"
                      disabled={busy || project.status === 'deleting'}
                      onClick={() => setMenuID((current) => current === project.id ? '' : project.id)}
                      aria-label={t('projects.actions.aria', { title: project.title })}
                      aria-haspopup="menu"
                      aria-expanded={menuID === project.id}
                      title={t('projects.actions.title')}
                    >
                      {controllingID === project.id ? <Loader2 className="spinIcon" size={14} /> : <MoreHorizontal size={16} />}
                    </button>
                    {menuID === project.id && (
                      <div className={`projectActionMenu ${menuOpensUp ? 'openUp' : ''}`} role="menu">
                        <div className="projectActionMenuHeader" role="presentation">
                          <span>{t('projects.actions.runtime')}</span>
                          <strong>{projectRuntimeState(project)}</strong>
                        </div>
                        <button role="menuitem" disabled={busy || !canStartPlayground(project)} onClick={() => void runPlaygroundAction(project, 'start')}><Play size={14} /> <span>{t('projects.start')}</span></button>
                        <button role="menuitem" disabled={busy || !canStopPlayground(project)} onClick={() => void runPlaygroundAction(project, 'stop')}><Square size={13} /> <span>{t('projects.stop')}</span></button>
                        <button role="menuitem" disabled={busy || !canRestartPlayground(project)} onClick={() => void runPlaygroundAction(project, 'restart')}><RotateCcw size={14} /> <span>{t('projects.restart')}</span></button>
                      </div>
                    )}
                  </div>
                </div>
              </>
            )}
          </div>
          );
        })}
      </div>
      <button className={capReached ? 'newProjectRow disabled' : 'newProjectRow'} onClick={onNew} disabled={capReached || busy}>
        {capReached ? <CircleAlert size={15} /> : <Plus size={15} />}
        {capReached ? t('projects.quotaFull') : t('projects.new')}
      </button>
    </div>
  );
}

export function HelpPanel({ markdown, onClose }: { markdown: string; onClose: () => void }) {
  const { t } = useI18n();
  return (
    <section className="inlinePanel helpInline">
      <div className="inlinePanelHeader">
        <div>
          <span className="eyebrow">{t('help.eyebrow')}</span>
          <strong>{t('help.title')}</strong>
        </div>
        <button className="projectDelete" onClick={onClose} aria-label={t('help.close')}><X size={15} /></button>
      </div>
      <MarkdownContent source={markdown} />
    </section>
  );
}

export function ServicePanel({ services, selectedName, busy, onSelect, onClose }: { services: ProjectService[]; selectedName?: string; busy: boolean; onSelect: (service: ProjectService) => void; onClose: () => void }) {
  const { t } = useI18n();
  const serviceCountLabel = t(services.length === 1 ? 'service.count.one' : 'service.count.many', { count: services.length });
  const serviceMeta = (service: ProjectService) => {
    return [service.type].filter(Boolean).join(' · ');
  };
  const serviceHost = (service: ProjectService) => {
    if (!service.url) return '';
    try {
      return new URL(service.url).host;
    } catch {
      return service.url.replace(/^https?:\/\//, '').split('/')[0] ?? '';
    }
  };
  return (
    <section className="inlinePanel serviceInline">
      <div className="inlinePanelHeader">
        <div>
          <span className="eyebrow">{t('service.preview')}</span>
          <strong>{selectedName ? t('service.selector', { name: selectedName }) : t('service.menu')}</strong>
          <small className="inlinePanelSubcopy">{serviceCountLabel}</small>
        </div>
        <button className="projectDelete" onClick={onClose} aria-label={t('service.close')}><X size={15} /></button>
      </div>
      <div className="serviceRows" role="radiogroup" aria-label={t('service.menu')}>
        {services.map((service) => {
          const selected = service.name === selectedName;
          const host = serviceHost(service);
          const meta = serviceMeta(service);
          return (
            <button
              key={service.name}
              className={selected ? 'serviceOption selected' : 'serviceOption'}
              disabled={busy}
              role="radio"
              aria-checked={selected}
              aria-label={t('service.show', { name: service.name })}
              onClick={() => onSelect(service)}
            >
              <span className="serviceOptionLead">
                <span className={`serviceOptionDot ${selected ? 'selected' : ''}`} aria-hidden="true" />
                <span className="serviceOptionCopy">
                  <span>{service.name}</span>
                  {meta && <small>{meta}</small>}
                  {host && <small className="serviceOptionUrl">{host}</small>}
                </span>
              </span>
              <em className={selected ? 'current' : service.authRequired ? 'locked' : ''}>{selected ? t('service.current') : service.authRequired ? t('service.authRequired') : t('service.public')}</em>
            </button>
          );
        })}
      </div>
    </section>
  );
}

function MarkdownContent({ source }: { source: string }) {
  return (
    <div className="helpMarkdown">
      {renderMarkdownBlocks(source)}
    </div>
  );
}

function renderMarkdownBlocks(source: string, keyPrefix = 'help'): ReactNode[] {
  const lines = source.replace(/\r\n/g, '\n').split('\n');
  return parseMarkdownLines(lines, 0, lines.length, keyPrefix).nodes;
}

function parseMarkdownLines(lines: string[], start: number, end: number, keyPrefix: string): { nodes: ReactNode[]; next: number } {
  const nodes: ReactNode[] = [];
  let index = start;
  while (index < end) {
    const raw = lines[index] ?? '';
    const line = raw.trim();
    if (!line) {
      index += 1;
      continue;
    }
    const details = line.match(/^<details(\s+open)?\s*>$/i);
    if (details) {
      const parsed = parseDetails(lines, index, end, `${keyPrefix}-${nodes.length}`);
      nodes.push(parsed.node);
      index = parsed.next;
      continue;
    }
    if (/^<\/details>$/i.test(line)) {
      break;
    }
    const heading = line.match(/^(#{1,3})\s+(.+)$/);
    if (heading) {
      const level = heading[1].length;
      const Tag = (`h${level}` as 'h1' | 'h2' | 'h3');
      nodes.push(<Tag key={`${keyPrefix}-${nodes.length}`}>{renderInlineMarkdown(heading[2], `${keyPrefix}-${nodes.length}-i`)}</Tag>);
      index += 1;
      continue;
    }
    if (/^[-*]\s+/.test(line)) {
      const items: ReactNode[] = [];
      while (index < end && /^[-*]\s+/.test((lines[index] ?? '').trim())) {
        const item = (lines[index] ?? '').trim().replace(/^[-*]\s+/, '');
        items.push(<li key={`${keyPrefix}-li-${index}`}>{renderInlineMarkdown(item, `${keyPrefix}-li-${index}-i`)}</li>);
        index += 1;
      }
      nodes.push(<ul key={`${keyPrefix}-${nodes.length}`}>{items}</ul>);
      continue;
    }
    const paragraph: string[] = [];
    while (index < end) {
      const nextLine = (lines[index] ?? '').trim();
      if (!nextLine || /^(#{1,3})\s+/.test(nextLine) || /^[-*]\s+/.test(nextLine) || /^<details(\s+open)?\s*>$/i.test(nextLine) || /^<\/details>$/i.test(nextLine)) {
        break;
      }
      paragraph.push(nextLine);
      index += 1;
    }
    nodes.push(<p key={`${keyPrefix}-${nodes.length}`}>{renderInlineMarkdown(paragraph.join(' '), `${keyPrefix}-${nodes.length}-i`)}</p>);
  }
  return { nodes, next: index };
}

function parseDetails(lines: string[], start: number, end: number, keyPrefix: string): { node: ReactNode; next: number } {
  const open = /\sopen\s*/i.test((lines[start] ?? '').trim());
  let index = start + 1;
  while (index < end && !(lines[index] ?? '').trim()) index += 1;
  let summary = '';
  const summaryMatch = (lines[index] ?? '').trim().match(/^<summary>(.+)<\/summary>$/i);
  if (summaryMatch) {
    summary = summaryMatch[1].trim();
    index += 1;
  }
  const contentStart = index;
  while (index < end && !/^<\/details>$/i.test((lines[index] ?? '').trim())) index += 1;
  const children = parseMarkdownLines(lines, contentStart, index, `${keyPrefix}-details`).nodes;
  const next = index < end ? index + 1 : index;
  return {
    next,
    node: (
      <details key={keyPrefix} open={open}>
        <summary>{renderInlineMarkdown(summary || 'Details', `${keyPrefix}-summary`)}</summary>
        <div>{children}</div>
      </details>
    )
  };
}

function renderInlineMarkdown(text: string, keyPrefix: string): ReactNode[] {
  const nodes: ReactNode[] = [];
  const pattern = /(\*\*[^*]+\*\*|`[^`]+`|\[[^\]]+\]\([^)]+\))/g;
  let lastIndex = 0;
  let match: RegExpExecArray | null;
  while ((match = pattern.exec(text)) !== null) {
    if (match.index > lastIndex) nodes.push(text.slice(lastIndex, match.index));
    const token = match[0];
    const link = token.match(/^\[([^\]]+)\]\(([^)]+)\)$/);
    if (link) {
      const href = safeMarkdownHref(link[2]);
      nodes.push(href
        ? <a key={`${keyPrefix}-${nodes.length}`} href={href} target={href.startsWith('http') ? '_blank' : undefined} rel={href.startsWith('http') ? 'noreferrer' : undefined}>{link[1]}</a>
        : <span key={`${keyPrefix}-${nodes.length}`}>{link[1]}</span>);
    } else if (token.startsWith('**')) {
      nodes.push(<strong key={`${keyPrefix}-${nodes.length}`}>{token.slice(2, -2)}</strong>);
    } else if (token.startsWith('`')) {
      nodes.push(<code key={`${keyPrefix}-${nodes.length}`}>{token.slice(1, -1)}</code>);
    }
    lastIndex = pattern.lastIndex;
  }
  if (lastIndex < text.length) nodes.push(text.slice(lastIndex));
  return nodes;
}

function safeMarkdownHref(value: string): string {
  const href = value.trim();
  if (/^(https?:|mailto:|\/|#)/i.test(href)) return href;
  return '';
}

export function UserMessageRow({ row }: { row: UserFeedRow }) {
  const { locale, t } = useI18n();
  const time = formatMessageTime(row.time, locale);
  const body = row.body.trim() || (row.attachments.length > 0 ? t('message.attachedFiles') : '');
  return (
    <article className="messageCard">
      <div className="messageMeta">
        <span>{t('message.sent')}</span>
        {time && <time dateTime={row.time}>{time}</time>}
      </div>
      {body && <div className="messageBody">{body}</div>}
      {row.attachments.length > 0 && <AttachmentGrid attachments={row.attachments} />}
    </article>
  );
}

function AttachmentGrid({ attachments }: { attachments: MessageAttachment[] }) {
  const { t } = useI18n();
  const [preview, setPreview] = useState<MessageAttachment | null>(null);
  return (
    <>
      <div className="messageAttachments" aria-label={t('message.attachments')}>
        {attachments.map((attachment) => {
          const kind = attachmentPreviewKind(attachment);
          const image = kind === 'image' && attachment.url;
          return (
            <button
              className={`messageAttachment ${image ? 'image' : ''}`}
              key={attachment.id}
              type="button"
              onClick={() => setPreview(attachment)}
              aria-label={t('message.preview.open', { name: attachment.filename })}
            >
              {image ? <img src={attachment.url} alt={attachment.filename} loading="lazy" /> : <Paperclip size={15} />}
              <span>{attachment.filename}</span>
            </button>
          );
        })}
      </div>
      {preview && <AttachmentPreviewModal attachment={preview} onClose={() => setPreview(null)} />}
    </>
  );
}

function AttachmentPreviewModal({ attachment, onClose }: { attachment: MessageAttachment; onClose: () => void }) {
  const { t } = useI18n();
  const kind = attachmentPreviewKind(attachment);
  const canPreview = Boolean(attachment.url && kind !== 'unsupported');
  const onScrimMouseDown = (event: MouseEvent<HTMLDivElement>) => {
    if (event.target === event.currentTarget) onClose();
  };

  return createPortal(
    <div className="modalScrim attachmentPreviewScrim" role="presentation" onMouseDown={onScrimMouseDown}>
      <section className={`attachmentPreviewDialog ${kind}`} role="dialog" aria-modal="true" aria-labelledby="attachment-preview-title">
        <button className="dialogClose" onClick={onClose} aria-label={t('message.preview.close')}><X size={16} /></button>
        <div className="attachmentPreviewHeader">
          <span className="eyebrow">{t('message.preview.eyebrow')}</span>
          <h2 id="attachment-preview-title">{attachment.filename}</h2>
        </div>
        <div className="attachmentPreviewBody">
          {canPreview && kind === 'image' && <img src={attachment.url} alt={attachment.filename} />}
          {canPreview && kind === 'pdf' && <iframe title={t('message.preview.pdfTitle', { name: attachment.filename })} src={attachment.url} loading="lazy" />}
          {!canPreview && (
            <div className="attachmentPreviewUnsupported">
              <Paperclip size={26} />
              <h3>{t('message.preview.unsupportedTitle')}</h3>
              <p>{t(attachment.url ? 'message.preview.unsupportedBody' : 'message.preview.unavailableBody')}</p>
            </div>
          )}
        </div>
        {attachment.url && (
          <div className="attachmentPreviewActions">
            <a className="ghostButton" href={attachment.url} download={attachment.filename}>
              <Download size={15} />
              {t('message.preview.download')}
            </a>
          </div>
        )}
      </section>
    </div>,
    document.body
  );
}

type AttachmentPreviewKind = 'image' | 'pdf' | 'unsupported';

function attachmentPreviewKind(attachment: MessageAttachment): AttachmentPreviewKind {
  if (!attachment.url) return 'unsupported';
  const contentType = (attachment.contentType ?? '').toLowerCase().split(';')[0].trim();
  const filename = attachment.filename.toLowerCase();
  if (isPreviewableImage(contentType, filename)) return 'image';
  if (contentType === 'application/pdf' || filename.endsWith('.pdf')) return 'pdf';
  return 'unsupported';
}

function isPreviewableImage(contentType: string, filename: string): boolean {
  return [
    'image/avif',
    'image/bmp',
    'image/gif',
    'image/jpeg',
    'image/png',
    'image/svg+xml',
    'image/webp',
    'image/x-icon',
    'image/vnd.microsoft.icon'
  ].includes(contentType) || /\.(avif|bmp|gif|ico|jpe?g|png|svg|webp)$/.test(filename);
}

export function AgentNotificationRow({ body, active, elapsedMs, elapsedStartedAt, tone }: { body: string; active?: boolean; elapsedMs?: number; elapsedStartedAt?: string; tone?: 'warning' }) {
  const { t } = useI18n();
  const [liveNow, setLiveNow] = useState(Date.now());
  const text = body.trim() || (active ? t('notification.receiving') : t('notification.canvasUpdated'));
  useEffect(() => {
    if (!active || !elapsedStartedAt) return;
    setLiveNow(Date.now());
    const timer = setInterval(() => setLiveNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [active, elapsedStartedAt]);
  const liveStartedAt = active ? Date.parse(elapsedStartedAt ?? '') : Number.NaN;
  const liveElapsedMs = !Number.isNaN(liveStartedAt) ? Math.max(0, liveNow - liveStartedAt) : undefined;
  const elapsed = formatElapsedDuration(liveElapsedMs ?? elapsedMs, elapsedDurationLabels(t));
  return (
    <div className={`notificationRow ${active ? 'active' : ''} ${tone === 'warning' ? 'warning' : ''}`} aria-live="polite">
      <div className="notificationBubble">
        <span className="notificationMark" aria-hidden="true">L</span>
        <span className="notificationSignal" aria-hidden="true">
          {active ? <Loader2 className="notificationSpinner" size={14} /> : tone === 'warning' ? <CircleAlert className="notificationWarning" size={14} /> : <Check className="notificationDone" size={14} />}
        </span>
        <span className="notificationCopy">
          <span className="notificationText">{text}</span>
          {elapsed && <small className="notificationElapsed">{elapsed}</small>}
        </span>
      </div>
    </div>
  );
}

export function EmptyCanvas({ title, body }: { title?: string; body?: string } = {}) {
  const { t } = useI18n();
  return (
    <div className="emptyPreview" aria-live="polite">
      <CanvasFrameDecor />
      <div className="emptyCopy">
        <h1>{title ?? t('empty.awaitingTitle')}</h1>
        <p>{body ?? t('empty.awaitingBody')}</p>
      </div>
    </div>
  );
}

export function CanvasLoader({ title, body, tone }: { title: string; body: string; tone?: 'error' }) {
  return (
    <div className={`emptyPreview canvasLoader ${tone === 'error' ? 'error' : ''}`} role="status" aria-live="polite" aria-busy={tone === 'error' ? undefined : true}>
      <CanvasFrameDecor loading />
      <div className="loaderRing" />
      <div className="loaderTelemetry" aria-hidden="true">
        <span />
        <span />
        <span />
      </div>
      <div className="emptyCopy">
        <h1>{title}</h1>
        <p>{body}</p>
      </div>
    </div>
  );
}

function CanvasFrameDecor({ loading }: { loading?: boolean } = {}) {
  return (
    <>
      <div className="stars" />
      <div className={`canvasSignalField ${loading ? 'loading' : ''}`} aria-hidden="true">
        <span />
        <span />
        <span />
      </div>
      <div className="reticle" />
      <div className="canvasHorizon" aria-hidden="true" />
    </>
  );
}

export function OnboardingGallery({ ready, onUsePrompt, onStart, onDismiss }: { ready: boolean; onUsePrompt: (prompt: string) => void; onStart: () => void; onDismiss: () => void }) {
  const { t } = useI18n();
  const [slide, setSlide] = useState(0);
  const examples = [
    {
      title: t('onboarding.card.productTitle'),
      body: t('onboarding.card.productBody'),
      prompt: t('onboarding.prompt.product')
    },
    {
      title: t('onboarding.card.opsTitle'),
      body: t('onboarding.card.opsBody'),
      prompt: t('onboarding.prompt.ops')
    },
    {
      title: t('onboarding.card.gameTitle'),
      body: t('onboarding.card.gameBody'),
      prompt: t('onboarding.prompt.game')
    }
  ];
  const slides = [
    {
      icon: <Sparkles size={18} />,
      title: t('onboarding.slide.buildTitle'),
      body: t('onboarding.slide.buildBody'),
      bullets: [
        t('onboarding.slide.buildBulletOne'),
        t('onboarding.slide.buildBulletTwo')
      ],
      suggestions: true
    },
    {
      icon: <Minimize2 size={18} />,
      title: t('onboarding.slide.modesTitle'),
      body: t('onboarding.slide.modesBody'),
      bullets: [
        t('onboarding.slide.modesBulletOne'),
        t('onboarding.slide.modesBulletTwo')
      ]
    },
    {
      icon: <MessageSquare size={18} />,
      title: t('onboarding.slide.messagesTitle'),
      body: t('onboarding.slide.messagesBody'),
      bullets: [
        t('onboarding.slide.messagesBulletOne'),
        t('onboarding.slide.messagesBulletTwo')
      ]
    },
    {
      icon: <CircleHelp size={18} />,
      title: t('onboarding.slide.helpTitle'),
      body: t('onboarding.slide.helpBody'),
      bullets: [
        t('onboarding.slide.helpBulletOne'),
        t('onboarding.slide.helpBulletTwo')
      ]
    },
    {
      icon: <FileOutput size={18} />,
      title: t('onboarding.slide.exportTitle'),
      body: t('onboarding.slide.exportBody'),
      bullets: [
        t('onboarding.slide.exportBulletOne'),
        t('onboarding.slide.exportBulletTwo')
      ]
    },
    {
      icon: <Download size={18} />,
      title: t('onboarding.slide.lifecycleTitle'),
      body: t('onboarding.slide.lifecycleBody'),
      bullets: [
        t('onboarding.slide.lifecycleBulletOne'),
        t('onboarding.slide.lifecycleBulletTwo')
      ]
    }
  ];
  const active = slides[slide];
  const previousSlide = () => setSlide((current) => (current + slides.length - 1) % slides.length);
  const nextSlide = () => setSlide((current) => (current + 1) % slides.length);
  return (
    <div className="emptyPreview onboardingGallery">
      <CanvasFrameDecor loading={!ready} />
      <div className="onboardingContent">
        <button className="onboardingClose" onClick={onDismiss} aria-label={t('onboarding.close')}>
          <X size={15} />
          <span>{t('onboarding.close')}</span>
        </button>
        <div className={`onboardingStatus ${ready ? 'ready' : 'loading'}`}>
          {ready ? <Check size={16} /> : <Loader2 size={16} className="spin" />}
          <span>{ready ? t('onboarding.status.ready') : t('onboarding.status.loading')}</span>
        </div>
        <div className="onboardingIntro">
          <span className="eyebrow">{t('onboarding.eyebrow')}</span>
          <h1>{t('onboarding.title')}</h1>
          <p>{t('onboarding.body')}</p>
          <div className="onboardingCueRow" aria-label={t('onboarding.actions')}>
            <span><Sparkles size={14} />{t('onboarding.action.prompt')}</span>
            <span><Play size={14} />{t('onboarding.action.start')}</span>
            <span><X size={14} />{t('onboarding.action.close')}</span>
          </div>
        </div>
        <div className="onboardingDeck" aria-live="polite">
          <div className="onboardingSlideCopy">
            <div className="onboardingSlideKicker">
              {active.icon}
              <span>{t('onboarding.slideCount', { current: slide + 1, total: slides.length })}</span>
            </div>
            <h2>{active.title}</h2>
            <p>{active.body}</p>
            <ul>
              {active.bullets.map((bullet) => <li key={bullet}>{bullet}</li>)}
            </ul>
            {active.suggestions && (
              <div className="onboardingCards" aria-label={t('onboarding.suggestions')}>
                {examples.map((example) => (
                  <button className="onboardingCard" key={example.title} onClick={() => onUsePrompt(example.prompt)}>
                    <Sparkles size={15} />
                    <span>
                      <strong>{example.title}</strong>
                      <em>{example.body}</em>
                    </span>
                  </button>
                ))}
              </div>
            )}
          </div>
          <div className="onboardingBlueprint" aria-hidden="true">
            <span className="blueprintNode primary" />
            <span className="blueprintNode secondary" />
            <span className="blueprintNode tertiary" />
            <span className="blueprintLine a" />
            <span className="blueprintLine b" />
            <span className="blueprintLine c" />
            <span className="blueprintPane large" />
            <span className="blueprintPane small" />
          </div>
        </div>
        <div className="onboardingFooter">
          <div className="onboardingNav">
            <button onClick={previousSlide} aria-label={t('onboarding.previous')}><ChevronLeft size={17} /></button>
            <div className="onboardingDots" aria-hidden="true">
              {slides.map((item, index) => <span className={index === slide ? 'active' : ''} key={item.title} />)}
            </div>
            <button onClick={nextSlide} aria-label={t('onboarding.next')}><ChevronRight size={17} /></button>
          </div>
          <button className="primaryButton onboardingStart" disabled={!ready} onClick={onStart}>
            {ready ? <Play size={16} /> : <Loader2 size={16} className="spin" />}
            {ready ? t('onboarding.start') : t('onboarding.waiting')}
          </button>
        </div>
      </div>
    </div>
  );
}

export function AppDialog({ dialog, onClose }: { dialog: AppDialogConfig; onClose: () => void }) {
  const { t } = useI18n();
  const [working, setWorking] = useState(false);
  const isConfirm = Boolean(dialog.onConfirm);
  const tone = dialog.tone ?? 'info';
  const confirmClass = tone === 'danger' ? 'dangerButton' : 'primaryButton';
  const confirmLabel = dialog.confirmLabel ?? (isConfirm ? t('common.confirm') : t('common.close'));
  const toneLabel = tone === 'danger' ? t('common.danger') : tone === 'warning' ? t('common.warning') : t('common.info');

  const handleConfirm = async () => {
    if (!dialog.onConfirm) {
      onClose();
      return;
    }
    setWorking(true);
    try {
      await dialog.onConfirm();
      onClose();
    } finally {
      setWorking(false);
    }
  };

  return (
    <div className="modalScrim appDialogScrim" role="presentation">
      <section className={`confirmDialog appDialog ${tone}`} role="dialog" aria-modal="true" aria-labelledby="app-dialog-title">
        <button className="dialogClose" onClick={onClose} aria-label={t('dialog.close')}><X size={16} /></button>
        <span className="eyebrow">{toneLabel}</span>
        <h2 id="app-dialog-title">{dialog.title}</h2>
        <p>{dialog.body}</p>
        <div className="dialogActions appDialogActions">
          {isConfirm && (
            <button className="ghostButton" disabled={working} onClick={onClose}>
              {dialog.cancelLabel ?? t('common.cancel')}
            </button>
          )}
          <button className={confirmClass} disabled={working} onClick={() => void handleConfirm()}>
            {working ? <Loader2 className="spinIcon" size={15} /> : null}
            {confirmLabel}
          </button>
        </div>
      </section>
    </div>
  );
}

export function ConfirmNewProject({ projectCap, projectCount, busy, onCancel, onConfirm }: { projectCap: number | null; projectCount: number; busy: boolean; onCancel: () => void; onConfirm: (title?: string) => void }) {
  const { t } = useI18n();
  const [title, setTitle] = useState('');
  const capReached = projectCap != null && projectCount >= projectCap;
  return (
    <div className="modalScrim">
      <section className="confirmDialog newProjectDialog" role="dialog" aria-modal="true" aria-labelledby="new-project-title">
        <button className="dialogClose" onClick={onCancel} aria-label={t('dialog.close')}><X size={16} /></button>
        <span className="eyebrow">{t('newProject.eyebrow')}</span>
        <h2 id="new-project-title">{t('newProject.title')}</h2>
        <p>{t(capReached ? 'newProject.capReachedBody' : 'newProject.body')}</p>
        {capReached ? (
          <div className="dialogNotice warning">
            <CircleAlert size={16} />
            <span>
              <strong>{t('newProject.capReached')}</strong>
              <small>{t('newProject.capReachedAction')}</small>
            </span>
          </div>
        ) : (
          <label className="dialogField">
            <span>{t('builder.project.name')}</span>
            <input className="dialogInput" value={title} maxLength={80} onChange={(event) => setTitle(event.target.value)} placeholder={t('newProject.placeholder', { number: projectCount + 1 })} />
          </label>
        )}
        {projectCap != null && <p className={`quotaLine ${capReached ? 'warning' : ''}`}>{t('newProject.quota', { count: projectCount, cap: projectCap })}</p>}
        <div className="dialogActions">
          <button className="ghostButton" onClick={onCancel}>{t('common.cancel')}</button>
          <button className="primaryButton" disabled={busy || capReached} onClick={() => onConfirm(title)}>
            {busy ? <Loader2 className="spinIcon" size={15} /> : <Plus size={15} />}
            {capReached ? t('newProject.capReached') : t('common.create')}
          </button>
        </div>
      </section>
    </div>
  );
}

export function ConfirmDeleteProject({ project, busy, onCancel, onConfirm }: { project: Project; busy: boolean; onCancel: () => void; onConfirm: () => void }) {
  const { t } = useI18n();
  const initial = (project.title.trim()[0] || 'L').toUpperCase();
  return (
    <div className="modalScrim">
      <section className="confirmDialog deleteProjectDialog" role="dialog" aria-modal="true" aria-labelledby="delete-project-title">
        <button className="dialogClose" disabled={busy} onClick={onCancel} aria-label={t('dialog.close')}><X size={16} /></button>
        <span className="eyebrow">{t('deleteProject.eyebrow')}</span>
        <h2 id="delete-project-title">{t('deleteProject.title')}</h2>
        <div className="dialogProjectSummary danger">
          <span className={`projectRowMark ${project.status}`} aria-hidden="true">{initial}</span>
          <span>
            <strong>{project.title}</strong>
            <small>{statusLabel(project.status, t)}</small>
          </span>
        </div>
        <p>{t('deleteProject.body', { title: project.title })}</p>
        <div className="dialogActions">
          <button className="ghostButton" onClick={onCancel}>{t('common.cancel')}</button>
          <button className="dangerButton" disabled={busy} onClick={onConfirm}>
            {busy ? <Loader2 className="spinIcon" size={15} /> : <Trash2 size={15} />}
            {t('common.delete')}
          </button>
        </div>
      </section>
    </div>
  );
}

export function ConfirmExportProject({ project, busy, busyMode, githubConnected, githubNeedsReconnect, onCancel, onGithub, onZip, onConnectGithub }: { project: Project; busy: boolean; busyMode: 'github' | 'zip' | ''; githubConnected: boolean; githubNeedsReconnect: boolean; onCancel: () => void; onGithub: (repoName: string, privateRepo: boolean) => void; onZip: () => void; onConnectGithub: () => void }) {
  const { t } = useI18n();
  const [repoName, setRepoName] = useState(defaultGithubRepoName(project.title));
  const [privateRepo, setPrivateRepo] = useState(true);
  const cleanName = repoName.trim();
  const valid = /^[A-Za-z0-9._-]{1,100}$/.test(cleanName);
  const archived = project.status === 'archived';
  const initial = (project.title.trim()[0] || 'L').toUpperCase();
  const githubLabel = !githubConnected ? t('exportProject.connectGithub') : githubNeedsReconnect ? t('exportProject.reconnectGithub') : t('exportProject.exportGithub');
  return (
    <div className="modalScrim">
      <section className="confirmDialog exportProjectDialog" role="dialog" aria-modal="true" aria-labelledby="export-project-title">
        <button className="dialogClose" disabled={busy} onClick={onCancel} aria-label={t('dialog.close')}><X size={16} /></button>
        <span className="eyebrow">{t('exportProject.eyebrow')}</span>
        <h2 id="export-project-title">{t('exportProject.title')}</h2>
        <div className="dialogProjectSummary">
          <span className={`projectRowMark ${project.status}`} aria-hidden="true">{initial}</span>
          <span>
            <strong>{project.title}</strong>
            <small>{statusLabel(project.status, t)}</small>
          </span>
        </div>
        <p>{t(archived ? 'exportProject.archivedBody' : 'exportProject.body', { title: project.title })}</p>
        <div className="dialogExportModes" aria-hidden="true">
          <span><Download size={14} /> {t('exportProject.zipHint')}</span>
          {!archived && <span><GitBranch size={14} /> {t(githubNeedsReconnect ? 'exportProject.githubReconnectHint' : 'exportProject.githubHint')}</span>}
        </div>
        {!archived && (
          <>
            <label className="dialogField">
              <span>{t('exportProject.repositoryName')}</span>
              <input className="dialogInput" value={repoName} maxLength={100} onChange={(event) => setRepoName(event.target.value)} placeholder="likeable-project" />
            </label>
            <label className="dialogCheck">
              <input type="checkbox" checked={privateRepo} onChange={(event) => setPrivateRepo(event.target.checked)} />
              <span>{t('exportProject.privateRepo')}</span>
            </label>
            {!valid && <p className="quotaLine">{t('exportProject.invalidName')}</p>}
          </>
        )}
        <div className="dialogActions">
          <button className="ghostButton" disabled={busy} onClick={onCancel}>{t('common.cancel')}</button>
          <button className="ghostButton" disabled={busy} onClick={onZip}>
            {busy && busyMode === 'zip' ? <Loader2 className="spinIcon" size={15} /> : <Download size={15} />}
            {t('common.zip')}
          </button>
          {!archived && (
            <button className="primaryButton" disabled={busy || (githubConnected && !githubNeedsReconnect && !valid)} onClick={() => {
              if (!githubConnected || githubNeedsReconnect) {
                onConnectGithub();
                return;
              }
              onGithub(cleanName, privateRepo);
            }}>
              {busy && busyMode === 'github' ? <Loader2 className="spinIcon" size={15} /> : <GitBranch size={15} />}
              {githubLabel}
            </button>
          )}
        </div>
      </section>
    </div>
  );
}

function defaultGithubRepoName(title: string) {
  return title
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9._-]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 100) || 'likeable-project';
}

export function Metric({ label, value }: { label: string; value: string }) {
  return (
    <div className="customerMetric">
      <span>{label}</span>
      <strong>{value}</strong>
    </div>
  );
}

export function DeleteAllAccountDialog({ email, busy, onCancel, onConfirm }: { email: string; busy: boolean; onCancel: () => void; onConfirm: (email: string) => void }) {
  const { t } = useI18n();
  const [typedEmail, setTypedEmail] = useState('');
  const confirmed = typedEmail.trim().toLowerCase() === email.trim().toLowerCase() && email.trim() !== '';
  return (
    <div className="modalScrim appDialogScrim" role="presentation">
      <section className="confirmDialog appDialog danger deleteAllDialog" role="dialog" aria-modal="true" aria-labelledby="delete-all-title">
        <button className="dialogClose" disabled={busy} onClick={onCancel} aria-label={t('dialog.close')}><X size={16} /></button>
        <span className="eyebrow">{t('deleteAll.eyebrow')}</span>
        <h2 id="delete-all-title">{t('deleteAll.title')}</h2>
        <p>{t('deleteAll.body')}</p>
        <p>{t('deleteAll.confirmInstruction', { email })}</p>
        <input
          className="dangerConfirmInput"
          autoFocus
          value={typedEmail}
          onChange={(event) => setTypedEmail(event.target.value)}
          placeholder={email}
          disabled={busy}
        />
        <div className="dialogActions appDialogActions">
          <button className="ghostButton" disabled={busy} onClick={onCancel}>{t('common.cancel')}</button>
          <button className="dangerButton" disabled={!confirmed || busy} onClick={() => onConfirm(typedEmail)}>
            {busy ? <Loader2 className="spinIcon" size={15} /> : <Trash2 size={15} />}
            {t('deleteAll.button')}
          </button>
        </div>
      </section>
    </div>
  );
}
