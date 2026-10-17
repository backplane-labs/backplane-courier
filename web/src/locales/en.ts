export default {
  channels: 'Channels', title: 'Delivery channels', description: 'The channels of this installation: mail, push, Web Push and every provider a plugin adds. Credentials never leave the service.',
  refresh: 'Refresh', loading: 'Loading', error: 'Could not load. Check the connection and the service.',
  channel: 'Channel', kind: 'Kind', activity: 'Activity', details: 'Details', limits: 'Limits', check: 'Check', checking: 'Checking…',
  checkOk: 'Works ({{took}})', checkFailed: 'Fails: {{error}}', checkHint: 'Reaches the provider without delivering: SMTP connect and authenticate, an FCM validate-only message, a Web Push key pair, a plugin\'s check.',
  default: 'Default', pluginUp: 'plugin serving', pluginDown: 'plugin down',
  problem: '{{channel}}: courier is not ready while the plugin is down or changed', audit: 'Every delivery is an audit record of courier (courier.delivered, courier.failed).', plugin: 'plugin {{name}}', pool: 'pool {{count}}', rate: '{{count}}/min', unbounded: 'no rate limit', timeout: 'timeout {{value}}',
  dryRun: 'Dry run: deliveries are recorded, nothing is sent.', noChannels: 'No channel is configured.',
  webPush: 'Web Push', webPushHelp: 'Browsers subscribe with the channel\'s public key (courier.WebPushKey); the product keeps their subscriptions and drops one when a push fails with code gone.',
  generateKeys: 'Generate VAPID keys', generateKeysHelp: 'A fresh pair for a webpush channel: the public key goes to browsers, the private key into the channel\'s secret. Courier keeps nothing.', generatedKeys: 'Generated keys',
  subscription: 'Subscription', subscriptionHelp: 'PushSubscription.toJSON() of a browser subscribed with this channel\'s public key.', payload: 'Payload (JSON the service worker gets)',
  sendTest: 'Send test push', sending: 'Sending…', sent: 'Accepted by the push service ({{took}})', gone: 'Gone: the browser unsubscribed or the subscription expired',
  missing: 'Page not found', back: 'Back to channels', standalone: 'Standalone · local fixtures', embedded: 'Embedded in Backplane', light: 'Light theme', dark: 'Dark theme',
};
