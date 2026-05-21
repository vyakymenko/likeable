export const BUILDER_MODE_KEY = 'likeable.builder.mode';
export const BASIC_CHAT_COLLAPSED_KEY = 'likeable.builder.basicChatCollapsed';
export const BASIC_CHAT_HEIGHT_KEY = 'likeable.builder.basicChatHeight';
export const COLLAPSED_CHAT_POSITION_KEY = 'likeable.builder.collapsedChatPosition';
export const SINGLE_VIEW_QUERY = '(max-width: 920px)';
export const MAX_ATTACHMENTS = 5;
export const LIKEABLE_NOTIFICATION_START = '[[LIKEABLE_NOTIFICATION_START]]';
export const LIKEABLE_NOTIFICATION_END = '[[LIKEABLE_NOTIFICATION_END]]';
export const ADMIN_CONFIG_SECTIONS = [
  {
    titleKey: 'admin.fibe.title',
    bodyKey: 'admin.fibe.body',
    keys: ['fibe_base_url', 'fibe_api_key']
  },
  {
    titleKey: 'admin.config.github.title',
    bodyKey: 'admin.config.github.body',
    keys: ['github_client_id', 'github_client_secret', 'github_username', 'github_token']
  },
  {
    titleKey: 'admin.config.google.title',
    bodyKey: 'admin.config.google.body',
    keys: ['google_client_id', 'google_client_secret']
  },
  {
    titleKey: 'admin.config.stripe.title',
    bodyKey: 'admin.config.stripe.body',
    keys: ['stripe_publishable_key', 'stripe_secret_key', 'stripe_price_id_1_hour', 'stripe_price_id_10_hours', 'stripe_price_id_100_hours', 'stripe_project_quota_price_id', 'stripe_webhook_secret']
  },
  {
    titleKey: 'admin.config.application.title',
    bodyKey: 'admin.config.application.body',
    keys: ['fibe_template_version_id', 'free_hours', 'free_hour_window_hours', 'project_cap', 'smtp_host', 'smtp_port', 'smtp_username', 'smtp_password', 'smtp_from_email', 'smtp_from_name', 'smtp_tls_mode']
  }
];
