export default {
  channels: 'Channels', title: 'Delivery channels', description: 'The channels of this installation: mail, push and every provider a plugin adds. Credentials never leave the service.',
  refresh: 'Refresh', loading: 'Loading', error: 'Could not load. Check the connection and the service.',
  channel: 'Channel', kind: 'Kind', activity: 'Activity', details: 'Details', limits: 'Limits', check: 'Check', checking: 'Checking…',
  checkOk: 'Works ({{took}})', checkFailed: 'Fails: {{error}}', checkHint: 'Reaches the provider without delivering: SMTP connect and authenticate, an FCM validate-only message, a plugin\'s check.',
  default: 'Default', plugin: 'plugin {{name}}', pool: 'pool {{count}}', rate: '{{count}}/min', unbounded: 'no rate limit', timeout: 'timeout {{value}}',
  dryRun: 'Dry run: deliveries are recorded, nothing is sent.', noChannels: 'No channel is configured.',
  missing: 'Page not found', back: 'Back to channels', standalone: 'Standalone · local fixtures', embedded: 'Embedded in Backplane', light: 'Light theme', dark: 'Dark theme',
};
