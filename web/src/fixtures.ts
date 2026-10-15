import { create } from '@bufbuild/protobuf';
import type { ClientRuntime, ClientConstructor, ClientState } from '@backplane-labs/backplane-client';
import { CheckChannelResponseSchema, ListChannelsResponseSchema, type CheckChannelRequest } from './gen/proto/courier/console/v1/admin_pb';

const state: ClientState = { connection: 'connected', session: null, connectionId: 1 };
/** Explicit local fixture of AdminService. Replace with BackplaneClient or your own adapter. */
export const fixtureClient: ClientRuntime = {
  getSnapshot: () => state, subscribe: () => () => {},
  client<T extends object>(constructor: ClientConstructor<T>): T {
    if (!('listChannels' in constructor.prototype)) throw new Error(`No fixture for ${constructor.name}`);
    return {
      async listChannels() { return create(ListChannelsResponseSchema, { dryRun: false, channels: [
        { name: 'mail', kind: 'mail', activity: 'SendMail', details: { host: 'smtp.example.com:587', tls: 'starttls', auth: 'shop', from: 'Shop <no-reply@shop.example>' }, pool: 8, ratePerMinute: 600, timeout: { seconds: 30n }, default: true },
        { name: 'push', kind: 'push', activity: 'SendPush', details: { project: 'shop-prod' }, pool: 16, timeout: { seconds: 15n }, default: true },
        { name: 'sms', kind: 'sms', plugin: 'smsc', activity: 'SendSms', details: { provider: 'smsc', login: 'shop', password: '***' }, problem: 'plugin unix:///run/smsc.sock: health: connection refused', pool: 4, timeout: { seconds: 10n }, default: true },
      ] }); },
      async checkChannel(request: CheckChannelRequest) { await new Promise((resolve) => setTimeout(resolve, 300)); return create(CheckChannelResponseSchema, request.name === 'sms' ? { ok: false, error: 'AUTH: wrong password', took: { nanos: 40_000_000 } } : { ok: true, took: { nanos: 12_000_000 } }); },
    } as unknown as T;
  },
};
