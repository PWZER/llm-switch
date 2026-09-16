import { useCallback, useEffect, useMemo, useState } from 'react';
import {
  App as AntApp, AutoComplete, Button, Drawer, Form, Input, Modal, Popconfirm,
  Select, Space, Table, Tag, Typography, theme,
} from 'antd';
import {
  ArrowDownOutlined, ArrowUpOutlined, PlusOutlined, SwapOutlined,
} from '@ant-design/icons';
import { api, Account, Model, ModelRoute, ModelRouteTarget, Channel, Provider } from '../api/client';
import { useLang } from '../i18n/i18n';

// Form/preview state for one failover target. provider_id is a UI-only
// cascading helper; only (channel_id, upstream_model, account_id) reaches
// the API. account_id pins the target to one account of the channel's
// provider; absent = the provider's account pool rotates.
interface TargetRow {
  provider_id?: number;
  channel_id?: number;
  upstream_model?: string;
  account_id?: number;
}

// Deduplicated upstream-model suggestions from one provider's model rows.
const upstreamModelOptions = (models: Model[], providerId?: number): { value: string }[] => {
  const seen = new Set<string>();
  const out: { value: string }[] = [];
  for (const m of models) {
    if (m.provider_id !== providerId) continue;
    const up = m.upstream_model || m.id;
    if (up && !seen.has(up)) {
      seen.add(up);
      out.push({ value: up });
    }
  }
  return out;
};

const channelLabel = (channels: Channel[], id?: number) => {
  const c = channels.find((x) => x.id === id);
  return c ? `${c.name} (${c.protocol})` : `#${id}`;
};

const accountLabel = (accounts: Account[], id?: number) => {
  if (!id) return '';
  const a = accounts.find((x) => x.id === id);
  return a ? (a.label || a.api_key_mask) : `#${id}`;
};

const toModelRouteTargets = (rows: TargetRow[]): ModelRouteTarget[] =>
  rows.map(({ channel_id, upstream_model, account_id }) => ({
    channel_id: channel_id as number,
    upstream_model: upstream_model as string,
    ...(account_id ? { account_id } : {}),
  }));

// Model routes are the hot-switch layer: an agent keeps model "main" forever while
// the admin re-points the target chain here. The FIRST healthy endpoint
// serves; the rest are automatic failover. Changes apply to the next request
// with no restart.
export default function ModelRoutes() {
  const { t } = useLang();
  const { message } = AntApp.useApp();
  const [rows, setRows] = useState<ModelRoute[]>([]);
  const [providers, setProviders] = useState<Provider[]>([]);
  const [channels, setChannels] = useState<Channel[]>([]);
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [models, setModels] = useState<Model[]>([]);
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [editing, setEditing] = useState<ModelRoute | null>(null);

  const load = useCallback(() => {
    api.get<ModelRoute[]>('/api/v1/model-routes')
      .then((list) => {
        setRows(list ?? []);
        // Keep the open drawer pointed at fresh data after remote mutations.
        setEditing((prev) => (prev ? ((list ?? []).find((a) => a.name === prev.name) ?? prev) : prev));
      })
      .catch((e) => message.error(e.message));
    api.get<Provider[]>('/api/v1/providers').then((x) => setProviders(x ?? [])).catch(() => {});
    api.get<Channel[]>('/api/v1/channels').then((x) => setChannels(x ?? [])).catch(() => {});
    api.get<Account[]>('/api/v1/accounts').then((x) => setAccounts(x ?? [])).catch(() => {});
    api.get<Model[]>('/api/v1/models').then((x) => setModels(x ?? [])).catch(() => {});
  }, [message]);
  useEffect(load, [load]);

  return (
    <div>
      <Space style={{ marginBottom: 16 }}>
        <Button
          type="primary"
          icon={<PlusOutlined />}
          onClick={() => {
            setEditing(null);
            setDrawerOpen(true);
          }}
        >
          {t('route.new')}
        </Button>
        <Button onClick={load}>{t('common.refresh')}</Button>
      </Space>
      <Table<ModelRoute>
        rowKey="name"
        dataSource={rows}
        columns={[
          {
            title: t('route.name'),
            dataIndex: 'name',
            render: (n: string) => <code>{n}</code>,
          },
          {
            title: t('route.targets'),
            render: (_, a) => (
              <Space direction="vertical" size={0}>
                {(a.targets ?? []).map((tgt, i) => (
                  <span key={i}>
                    <Tag color={i === 0 ? 'green' : 'default'}>
                      {i === 0 ? t('route.primary') : `#${i + 1}`}
                    </Tag>
                    {channelLabel(channels, tgt.channel_id)} → <code>{tgt.upstream_model}</code>
                    {tgt.account_id ? (
                      <Tag color="blue" style={{ marginInlineStart: 6 }}>
                        {t('route.pinnedAccount')}: {accountLabel(accounts, tgt.account_id)}
                      </Tag>
                    ) : null}
                  </span>
                ))}
              </Space>
            ),
          },
          {
            title: t('common.actions'),
            render: (_, a) => (
              <Space>
                <Button
                  size="small"
                  icon={<SwapOutlined />}
                  onClick={() => {
                    setEditing(a);
                    setDrawerOpen(true);
                  }}
                >
                  {t('route.switchTarget')}
                </Button>
                <Popconfirm
                  title={t('route.deleteConfirm')}
                  onConfirm={async () => {
                    await api.del(`/api/v1/model-routes/${a.name}`);
                    load();
                  }}
                >
                  <Button danger size="small">{t('common.delete')}</Button>
                </Popconfirm>
              </Space>
            ),
          },
        ]}
      />
      <ModelRouteDrawer
        open={drawerOpen}
        route={editing}
        channels={channels}
        providers={providers}
        accounts={accounts}
        models={models}
        onClose={() => setDrawerOpen(false)}
        onChanged={load}
      />
    </div>
  );
}

// One drawer serves both create (targets buffer locally, persisted in one
// submit) and manage (every mutation PUTs the whole chain immediately) —
// same pattern as ProviderDrawer.
function ModelRouteDrawer({
  open,
  route,
  channels,
  providers,
  accounts,
  models,
  onClose,
  onChanged,
}: {
  open: boolean;
  route: ModelRoute | null;
  channels: Channel[];
  providers: Provider[];
  accounts: Account[];
  models: Model[];
  onClose: () => void;
  onChanged: () => void;
}) {
  const { t } = useLang();
  const { message } = AntApp.useApp();
  const { token } = theme.useToken();
  const isCreate = route == null;

  // Create-mode local buffers.
  const [name, setName] = useState('');
  const [localTargets, setLocalTargets] = useState<TargetRow[]>([]);
  const [creating, setCreating] = useState(false);

  // Add/edit modal state (both modes).
  const [modalOpen, setModalOpen] = useState(false);
  const [editIdx, setEditIdx] = useState<number | null>(null);

  useEffect(() => {
    if (!open) return;
    setName('');
    setLocalTargets([]);
  }, [open, route?.name]); // eslint-disable-line react-hooks/exhaustive-deps

  // The form only offers routable targets — enabled channels of enabled
  // providers, the same set the engine snapshot routes to.
  const routableChannels = useMemo(() => {
    const enabledProviders = new Set(providers.filter((p) => p.enabled).map((p) => p.id));
    return channels.filter((c) => c.enabled && enabledProviders.has(c.provider_id));
  }, [providers, channels]);

  const providerOptions = useMemo(() => {
    const seen = new Set<number>();
    const out: { value: number; label: string }[] = [];
    for (const c of routableChannels) {
      if (!seen.has(c.provider_id)) {
        seen.add(c.provider_id);
        out.push({ value: c.provider_id, label: c.provider_name });
      }
    }
    return out;
  }, [routableChannels]);

  // Unified view of the chain being edited; create mode uses the buffer.
  const targets: TargetRow[] = isCreate
    ? localTargets
    : (route?.targets ?? []).map((tgt) => ({
        provider_id: channels.find((c) => c.id === tgt.channel_id)?.provider_id,
        channel_id: tgt.channel_id,
        upstream_model: tgt.upstream_model,
        account_id: tgt.account_id,
      }));

  const submitTargets = async (next: ModelRouteTarget[]) => {
    if (!route) return;
    try {
      await api.put(`/api/v1/model-routes/${route.name}`, { name: route.name, targets: next });
      message.success(`"${route.name}" ${t('route.switched')}`);
      onChanged();
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    }
  };

  // Single commit path for add/edit/remove/move: create mode mutates the
  // local buffer, manage mode PUTs the whole chain immediately.
  const commit = (next: TargetRow[]) => {
    if (isCreate) {
      setLocalTargets(next);
      return;
    }
    submitTargets(toModelRouteTargets(next));
  };

  const move = (i: number, dir: -1 | 1) => {
    const j = i + dir;
    if (j < 0 || j >= targets.length) return;
    const next = [...targets];
    [next[i], next[j]] = [next[j], next[i]];
    commit(next);
  };

  const submitCreate = async () => {
    if (!name.trim()) {
      message.warning(t('route.nameRequired'));
      return;
    }
    setCreating(true);
    try {
      await api.put(`/api/v1/model-routes/${name.trim()}`, {
        name: name.trim(),
        targets: toModelRouteTargets(localTargets),
      });
      message.success(`"${name.trim()}" ${t('route.switched')}`);
      onChanged();
      onClose();
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    } finally {
      setCreating(false);
    }
  };

  return (
    <Drawer
      title={isCreate ? t('route.new') : `${t('route.manage')} — ${route.name}`}
      open={open}
      onClose={onClose}
      width={960}
      extra={
        isCreate && (
          <Button type="primary" loading={creating} onClick={submitCreate}>
            {t('common.create')}
          </Button>
        )
      }
    >
      {open && (
        <>
          <Typography.Title level={5}>{t('route.name')}</Typography.Title>
          {isCreate ? (
            <Input
              placeholder={t('route.namePlaceholder')}
              value={name}
              onChange={(e) => setName(e.target.value)}
              style={{ width: 280 }}
            />
          ) : (
            <Input value={route.name} disabled style={{ width: 280 }} />
          )}

          <Typography.Title level={5} style={{ marginTop: 24 }}>
            {t('route.targets')}
          </Typography.Title>
          <Typography.Paragraph type="secondary" style={{ marginBottom: 12 }}>
            {t('route.chainTip')}
          </Typography.Paragraph>
          <Button
            size="small"
            icon={<PlusOutlined />}
            style={{ marginBottom: 12 }}
            onClick={() => {
              setEditIdx(null);
              setModalOpen(true);
            }}
          >
            {t('route.addTarget')}
          </Button>
          <Space direction="vertical" size="middle" style={{ width: '100%' }}>
            {targets.map((tgt, i) => (
              <div
                key={i}
                style={{ border: `1px solid ${token.colorBorderSecondary}`, borderRadius: 8, padding: 12 }}
              >
                <Space style={{ width: '100%', justifyContent: 'space-between' }}>
                  <Space>
                    <Tag color={i === 0 ? 'green' : 'default'} style={{ marginInlineEnd: 0 }}>
                      {i === 0 ? t('route.primary') : `#${i + 1}`}
                    </Tag>
                    <Typography.Text>{channelLabel(channels, tgt.channel_id)}</Typography.Text>
                    <Typography.Text type="secondary">→</Typography.Text>
                    <Typography.Text code>{tgt.upstream_model}</Typography.Text>
                    {tgt.account_id ? (
                      <Tag color="blue">
                        {t('route.pinnedAccount')}: {accountLabel(accounts, tgt.account_id)}
                      </Tag>
                    ) : null}
                  </Space>
                  <Space>
                    <Button size="small" icon={<ArrowUpOutlined />} disabled={i === 0} onClick={() => move(i, -1)} />
                    <Button
                      size="small"
                      icon={<ArrowDownOutlined />}
                      disabled={i === targets.length - 1}
                      onClick={() => move(i, 1)}
                    />
                    <Button
                      size="small"
                      onClick={() => {
                        setEditIdx(i);
                        setModalOpen(true);
                      }}
                    >
                      {t('common.edit')}
                    </Button>
                    <Popconfirm
                      title={t('common.remove') + '?'}
                      onConfirm={() => commit(targets.filter((_, j) => j !== i))}
                    >
                      <Button danger size="small">{t('common.remove')}</Button>
                    </Popconfirm>
                  </Space>
                </Space>
              </div>
            ))}
            {targets.length === 0 && (
              <Typography.Text type="secondary">{t('route.noTargets')}</Typography.Text>
            )}
          </Space>
          <TargetModal
            open={modalOpen}
            initial={editIdx != null ? targets[editIdx] : null}
            routableChannels={routableChannels}
            providerOptions={providerOptions}
            accounts={accounts}
            models={models}
            onSubmit={(v) => {
              commit(editIdx != null ? targets.map((x, j) => (j === editIdx ? v : x)) : [...targets, v]);
              setModalOpen(false);
            }}
            onClose={() => setModalOpen(false)}
          />
        </>
      )}
    </Drawer>
  );
}

// Dumb add/edit form for one target: provider → endpoint → model cascade,
// plus an optional pinned account. The parent decides persistence via onSubmit.
function TargetModal({
  open,
  initial,
  routableChannels,
  providerOptions,
  accounts,
  models,
  onSubmit,
  onClose,
}: {
  open: boolean;
  initial: TargetRow | null;
  routableChannels: Channel[];
  providerOptions: { value: number; label: string }[];
  accounts: Account[];
  models: Model[];
  onSubmit: (v: TargetRow) => void;
  onClose: () => void;
}) {
  const { t } = useLang();
  const [form] = Form.useForm<TargetRow>();
  const providerId = Form.useWatch('provider_id', form);

  useEffect(() => {
    if (!open) return;
    form.setFieldsValue(initial ?? { provider_id: undefined, channel_id: undefined, upstream_model: '', account_id: undefined });
  }, [open, initial, form]);

  const channelOptions = routableChannels
    .filter((c) => c.provider_id === providerId)
    .map((c) => ({ value: c.id, label: `${c.name} (${c.protocol})` }));
  const modelOptions = upstreamModelOptions(models, providerId);
  const accountOptions = accounts
    .filter((a) => a.provider_id === providerId)
    .map((a) => ({
      value: a.id,
      label: `${a.label || a.api_key_mask} (${a.api_key_mask})${a.enabled ? '' : ' (off)'}`,
    }));

  return (
    <Modal
      title={initial ? t('common.edit') : t('route.addTarget')}
      open={open}
      onCancel={onClose}
      onOk={() => form.submit()}
      destroyOnClose
      width={560}
    >
      <Form form={form} layout="vertical" onFinish={(v) => onSubmit(v)}>
        <Form.Item name="provider_id" label={t('route.provider')} rules={[{ required: true }]}>
          <Select
            options={providerOptions}
            placeholder={t('route.provider')}
            onChange={() => {
              form.setFieldValue('channel_id', undefined);
              form.setFieldValue('upstream_model', '');
              form.setFieldValue('account_id', undefined);
            }}
          />
        </Form.Item>
        <Form.Item name="channel_id" label={t('route.targetChannel')} rules={[{ required: true }]}>
          <Select
            options={channelOptions}
            placeholder={t('route.targetChannel')}
            onChange={() => form.setFieldValue('upstream_model', '')}
          />
        </Form.Item>
        <Form.Item name="upstream_model" label={t('route.upstreamModel')} rules={[{ required: true }]}>
          <AutoComplete
            options={modelOptions}
            placeholder={t('route.upstreamModel')}
            filterOption={(input, option) =>
              String(option?.value ?? '').toLowerCase().includes(input.toLowerCase())
            }
          />
        </Form.Item>
        <Form.Item name="account_id" label={t('route.pinnedAccount')} tooltip={t('route.pinnedAccountTip')}>
          <Select
            allowClear
            options={accountOptions}
            placeholder={t('route.poolRotation')}
          />
        </Form.Item>
      </Form>
    </Modal>
  );
}
