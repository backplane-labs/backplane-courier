import { create } from '@bufbuild/protobuf';
import type { ClientRuntime, ClientConstructor, ClientState } from '@backplane-labs/backplane-client';
import {
  CheckChannelResponseSchema, GenerateWebPushKeysResponseSchema, ListChannelsResponseSchema, TestWebPushResponseSchema, type CheckChannelRequest, type TestWebPushRequest,
} from './gen/proto/courier/console/v1/admin_pb';

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
        { name: 'web', kind: 'webpush', activity: 'SendWebPush', details: { subject: 'mailto:ops@shop.example', public_key: 'BNbxGYNMhEIi9zrneh7mqV4oUanjLUK3m-mYZBc62frMKrEoMk88r3Lk596T0ck9xlT-Pu3fN-hHw9iiqoX5Njs', hosts: "the browsers' push services" }, pool: 8, timeout: { seconds: 30n }, default: true },
        { name: 'sms', kind: 'sms', plugin: 'smsc', activity: 'SendSms', details: { provider: 'smsc', login: 'shop', password: '***' }, problem: 'plugin unix:///run/smsc.sock: health: connection refused', pool: 4, timeout: { seconds: 10n }, default: true },
      ] }); },
      async generateWebPushKeys() { return create(GenerateWebPushKeysResponseSchema, { publicKey: 'BOr4R0lQh2xH6r5mE0QvH3U6x3oVfOq7tQ1m3g0uY1sW2nZb8cD4eF5gH6iJ7kL8mN9oP0qR1sT2uV3wX4yZ5aA', privateKey: 'q1W2e3R4t5Y6u7I8o9P0a1S2d3F4g5H6j7K8l9Z0x1C' }); },
      async testWebPush(request: TestWebPushRequest) { await new Promise((resolve) => setTimeout(resolve, 300)); return create(TestWebPushResponseSchema, request.subscription.includes('gone') ? { ok: false, code: 'gone', error: 'gone: push service answered 410 Gone', took: { nanos: 90_000_000 } } : { ok: true, id: 'https://updates.push.services.mozilla.com/m/1', took: { nanos: 80_000_000 } }); },
      async checkChannel(request: CheckChannelRequest) { await new Promise((resolve) => setTimeout(resolve, 300)); return create(CheckChannelResponseSchema, request.name === 'sms' ? { ok: false, error: 'AUTH: wrong password', took: { nanos: 40_000_000 } } : { ok: true, took: { nanos: 12_000_000 } }); },
    } as unknown as T;
  },
};
