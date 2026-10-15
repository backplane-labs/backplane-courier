import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { create } from '@bufbuild/protobuf';
import type { Duration } from '@bufbuild/protobuf/wkt';
import { useClient } from '@backplane-labs/backplane-react';
import { definePlugin, usePluginContext, usePluginNavigate } from '@backplane-labs/backplane-plugin-sdk';
import { ConnectionNotice, usePlatformAction, usePlatformQuery } from '@backplane-labs/backplane-platform-ui';
import {
  Badge, Button, EmptyState, PageHeader, Panel, Skeleton, Stack, StatusBadge, Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from '@backplane-labs/backplane-ui';
import { AdminServiceClient } from './gen/proto/courier/console/v1/admin_ws_pb';
import { CheckChannelRequestSchema, ListChannelsRequestSchema, type Channel } from './gen/proto/courier/console/v1/admin_pb';
import navigation from './navigation';

const mono = { fontFamily: 'var(--font-mono)' } as const;
const muted = { color: 'var(--muted-foreground)', fontSize: 12 } as const;
const millis = (value?: Duration) => value ? `${Math.round(Number(value.seconds) * 1000 + value.nanos / 1e6)}ms` : '';
const seconds = (value?: Duration) => value ? `${Number(value.seconds)}s` : '—';
function useText() { return useTranslation('module.courier').t; }

/** Every channel, its provider details and a check. */
function Channels() {
  const t = useText(), context = usePluginContext(), client = useClient(AdminServiceClient);
  const [tick, setTick] = useState(0);
  useEffect(() => { const id = setInterval(() => setTick((n) => n + 1), 10_000); return () => clearInterval(id); }, []);
  const state = usePlatformQuery(`courier:channels:${tick}`, (signal) => client.listChannels(create(ListChannelsRequestSchema), { signal }), { keep: true });
  return <Stack gap={16}><ConnectionNotice />
    <PageHeader title={t('title')} description={t('description')} meta={<Badge variant="outline">{t(context.environment)}</Badge>}
      actions={<Button variant="outline" size="sm" onClick={state.refresh}>{t('refresh')}</Button>} />
    {state.value?.dryRun && <p role="status" style={{ margin: 0 }}><StatusBadge tone="warning">{t('dryRun')}</StatusBadge></p>}
    {state.value?.channels.filter((c) => c.problem).map((c) => <p key={c.name} role="alert" style={{ margin: 0 }}>
      <StatusBadge tone="danger">{t('problem', { channel: c.name })}</StatusBadge> <span style={{ fontSize: 13 }}>{c.problem}</span></p>)}
    <Panel title={t('channels')} count={state.value?.channels.length} flush footer={<span style={muted}>{t('audit')}</span>}>
      {state.error !== undefined ? <EmptyState title={t('error')} role="alert" /> : !state.value
        ? <Stack gap={8} style={{ padding: 12 }} role="status" aria-label={t('loading')}><Skeleton style={{ height: 28 }} /><Skeleton style={{ height: 28 }} /></Stack>
        : !state.value.channels.length ? <EmptyState title={t('noChannels')} />
        : <Table><TableHeader><TableRow><TableHead>{t('channel')}</TableHead><TableHead>{t('kind')}</TableHead><TableHead>{t('activity')}</TableHead><TableHead>{t('details')}</TableHead><TableHead>{t('limits')}</TableHead><TableHead>{t('check')}</TableHead></TableRow></TableHeader>
          <TableBody>{state.value.channels.map((channel) => <ChannelRow key={channel.name} channel={channel} />)}</TableBody></Table>}
    </Panel>
  </Stack>;
}

function ChannelRow({ channel }: { channel: Channel }) {
  const t = useText(), client = useClient(AdminServiceClient), check = usePlatformAction<{ ok: boolean; error: string; took: string }>();
  return <TableRow>
    <TableCell style={mono}>{channel.name}{channel.default && <Badge variant="outline" style={{ marginLeft: 8 }}>{t('default')}</Badge>}</TableCell>
    <TableCell><Stack gap={4}><Stack direction="row" gap={4}><Badge variant="outline">{channel.kind}</Badge>{channel.plugin && <span style={muted}>{t('plugin', { name: channel.plugin })}</span>}</Stack>
      {channel.plugin && (channel.problem
        ? <StatusBadge tone="danger" title={channel.problem}>{t('pluginDown')}</StatusBadge>
        : <StatusBadge tone="success">{t('pluginUp')}</StatusBadge>)}</Stack></TableCell>
    <TableCell style={mono}>courier.{channel.activity}</TableCell>
    <TableCell><Stack gap={2}>{Object.entries(channel.details).sort().map(([key, value]) => <span key={key} style={{ fontSize: 12 }}><span style={muted}>{key}</span> <span style={mono}>{value}</span></span>)}</Stack></TableCell>
    <TableCell style={muted}>{t('pool', { count: channel.pool })} · {channel.ratePerMinute ? t('rate', { count: channel.ratePerMinute }) : t('unbounded')} · {t('timeout', { value: seconds(channel.timeout) })}</TableCell>
    <TableCell><Stack gap={4} direction="row" style={{ alignItems: 'center' }}>
      <Button size="sm" variant="outline" disabled={check.pending || check.disabled} title={t('checkHint')} onClick={() => void check.run(async (signal) => {
        const response = await client.checkChannel(create(CheckChannelRequestSchema, { name: channel.name }), { signal });
        return { ok: response.ok, error: response.error, took: millis(response.took) };
      })}>{check.pending ? t('checking') : t('check')}</Button>
      {check.value && <StatusBadge tone={check.value.ok ? 'success' : 'danger'}>{check.value.ok ? t('checkOk', { took: check.value.took }) : t('checkFailed', { error: check.value.error })}</StatusBadge>}
      {check.error !== undefined && <span role="alert" style={{ color: 'var(--destructive)', fontSize: 12 }}>{t('error')}</span>}
    </Stack></TableCell>
  </TableRow>;
}

function Missing() {
  const t = useText(), navigate = usePluginNavigate();
  return <Panel><EmptyState title={<h1 style={{ margin: 0, fontSize: 16, fontWeight: 500 }}>{t('missing')}</h1>} action={<Button size="sm" variant="outline" onClick={() => navigate('')}>{t('back')}</Button>} /></Panel>;
}
export const definition = definePlugin({ ...navigation, routes: [{ index: true, element: <Channels /> }, { path: '*', element: <Missing /> }] });
