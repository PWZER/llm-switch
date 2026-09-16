import { useCallback, useEffect, useState, type ReactNode } from 'react';
import {
  App as AntApp, Button, Drawer, Form, Input, InputNumber, Modal, Popconfirm, Select, Space,
  Switch, Table, Tag, Typography,
} from 'antd';
import { PlusOutlined, SettingOutlined } from '@ant-design/icons';
import { api, Channel, ChannelModel, Provider, ProviderKey } from '../api/client';
import { useLang } from '../i18n/i18n';
import { endpointPresets, presetKey, type EndpointPreset } from '../data/presets';

// Merged provider management: one provider (account + shared key pool) hosts
// one or more protocol endpoints, each with its own model bindings and
// failover priority. Replaces the old Providers + Channels page pair.

export default function Providers() {
  const { t } = useLang();
  const { message } = AntApp.useApp();
  const [providers, setProviders] = useState<Provider[]>([]);
  const [channels, setChannels] = useState<Channel[]>([]);
  const [managing, setManaging] = useState<Provider | null>(null);
  const [creating, setCreating] = useState(false);

  const load = useCallback(() => {
    api.get<Provider[]>('/api/v1/providers').then((x) => setProviders(x ?? [])).catch((e) => message.error(e.message));
    api.get<Channel[]>('/api/v1/channels').then((x) => setChannels(x ?? [])).catch(() => {});
  }, [message]);
  useEffect(load, [load]);

  const toggle = async (p: Provider, enabled: boolean) => {
    await api.put(`/api/v1/providers/${p.id}`, { enabled }).catch(() => message.error('failed'));
    load();
  };

  const remove = async (p: Provider) => {
    await api.del(`/api/v1/providers/${p.id}`).catch((e) => message.error(e.message));
    message.success(t('prov.deleted'));
    load();
  };

  const channelName = (id: number) => channels.find((c) => c.id === id)?.name ?? `#${id}`;
  void channelName;

  return (
    <div>
      <Space style={{ marginBottom: 16 }}>
        <Button type="primary" icon={<PlusOutlined />} onClick={() => setCreating(true)}>
          {t('prov.new')}
        </Button>
        <Button onClick={load}>{t('common.refresh')}</Button>
      </Space>
      <Table<Provider>
        rowKey="id"
        dataSource={providers}
        columns={[
          { title: t('common.name'), dataIndex: 'name' },
          {
            title: t('common.enabled'),
            dataIndex: 'enabled',
            render: (v: boolean, p) => <Switch checked={v} onChange={(x) => toggle(p, x)} />,
          },
          {
            title: t('prov.endpoints'),
            render: (_, p) => {
              const list = channels.filter((c) => c.provider_id === p.id);
              if (list.length === 0) return <Typography.Text type="secondary">-</Typography.Text>;
              return (
                <Space wrap>
                  {list.map((c) => (
                    <Tooltiped key={c.id} title={`${c.base_url}${c.chat_path} · ${c.models?.length ?? 0} models`}>
                      <Tag color={c.protocol === 'openai' ? 'green' : 'orange'}>
                        {c.protocol}
                        {!c.enabled ? ' (off)' : ''}
                      </Tag>
                    </Tooltiped>
                  ))}
                </Space>
              );
            },
          },
          {
            title: t('common.actions'),
            width: 200,
            render: (_, p) => (
              <Space>
                <Button size="small" icon={<SettingOutlined />} onClick={() => setManaging(p)}>
                  {t('prov.manage')}
                </Button>
                <Popconfirm title={t('prov.deleteConfirm')} onConfirm={() => remove(p)}>
                  <Button danger size="small">{t('common.delete')}</Button>
                </Popconfirm>
              </Space>
            ),
          },
        ]}
      />

      <CreateDrawer
        open={creating}
        onClose={() => setCreating(false)}
        onDone={() => {
          setCreating(false);
          message.success(t('prov.created'));
          load();
        }}
      />

      <ManageDrawer
        provider={managing}
        channels={channels}
        onClose={() => setManaging(null)}
        onChanged={load}
      />
    </div>
  );
}

function Tooltiped({ title, children }: { title: string; children: ReactNode }) {
  return (
    <Typography.Text title={title} style={{ cursor: 'default' }}>
      {children}
    </Typography.Text>
  );
}

// — create drawer: provider + first endpoint in one step -------------------

interface CreateForm {
  name: string;
  presetKey?: string;
  base_url?: string;
  chat_path?: string;
  auth_style?: 'bearer' | 'x-api-key';
}

function CreateDrawer({ open, onClose, onDone }: { open: boolean; onClose: () => void; onDone: () => void }) {
  const { t } = useLang();
  const { message } = AntApp.useApp();
  const [form] = Form.useForm<CreateForm>();
  const [preset, setPreset] = useState<EndpointPreset | null>(null);

  const applyPreset = (key?: string) => {
    const p = endpointPresets.find((x) => presetKey(x) === key) ?? null;
    setPreset(p);
    form.setFieldsValue(
      p
        ? { base_url: p.base_url, chat_path: p.chat_path, auth_style: p.auth_style }
        : { base_url: undefined, chat_path: undefined, auth_style: 'bearer' },
    );
  };

  const submit = async (v: CreateForm) => {
    try {
      const created = await api.post<{ id: number }>('/api/v1/providers', { name: v.name });
      if (v.base_url) {
        await api.post('/api/v1/channels', {
          provider_id: created.id,
          name: preset ? `${preset.vendor.toLowerCase()}-${preset.protocol}` : 'default',
          protocol: preset?.protocol ?? 'openai',
          base_url: v.base_url,
          chat_path: v.chat_path,
          auth_style: v.auth_style ?? 'bearer',
          priority: 10,
          weight: 1,
          enabled: true,
          passthrough: true,
          models: [],
        });
      }
      onDone();
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    }
  };

  return (
    <Drawer
      title={t('prov.new')}
      open={open}
      onClose={onClose}
      width={480}
      extra={
        <Button type="primary" onClick={() => form.submit()}>
          {t('common.create')}
        </Button>
      }
    >
      <Form form={form} layout="vertical" onFinish={submit}>
        <Form.Item name="presetKey" label={t('prov.preset')} tooltip={t('prov.presetTip')}>
          <Select
            allowClear
            placeholder={t('prov.presetCustom')}
            onChange={applyPreset}
            options={endpointPresets.map((p) => ({ value: presetKey(p), label: presetKey(p) }))}
          />
        </Form.Item>
        <Form.Item name="name" label={t('common.name')} rules={[{ required: true }]}>
          <Input placeholder={t('prov.namePlaceholder')} />
        </Form.Item>
        <Typography.Title level={5} style={{ marginTop: 8 }}>
          {t('prov.endpoint')}
        </Typography.Title>
        <Form.Item name="base_url" label={t('prov.baseUrl')}>
          <Input placeholder="https://api.deepseek.com" />
        </Form.Item>
        <Form.Item name="chat_path" label={t('prov.chatPath')} tooltip={t('prov.chatPathTip')}>
          <Input placeholder="/chat/completions" />
        </Form.Item>
        <Form.Item name="auth_style" label={t('prov.authStyle')} initialValue="bearer">
          <Select
            style={{ width: 200 }}
            options={[
              { value: 'bearer', label: t('prov.bearer') },
              { value: 'x-api-key', label: t('prov.xApiKey') },
            ]}
          />
        </Form.Item>
        <Typography.Text type="secondary">
          {t('prov.keys')} / {t('prov.bindings')}: {t('prov.manage')} →
        </Typography.Text>
      </Form>
    </Drawer>
  );
}

// — manage drawer: shared keys + endpoint cards ----------------------------

function ManageDrawer({
  provider,
  channels,
  onClose,
  onChanged,
}: {
  provider: Provider | null;
  channels: Channel[];
  onClose: () => void;
  onChanged: () => void;
}) {
  const { t } = useLang();
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<Channel | null>(null);

  const list = provider ? channels.filter((c) => c.provider_id === provider.id) : [];

  return (
    <Drawer
      title={`${t('prov.manage')} — ${provider?.name ?? ''}`}
      open={!!provider}
      onClose={onClose}
      width={720}
    >
      {provider && (
        <>
          <Typography.Title level={5}>{t('prov.keys')}</Typography.Title>
          <KeysTable providerId={provider.id} />

          <Typography.Title level={5} style={{ marginTop: 24 }}>
            {t('prov.endpoints')}
          </Typography.Title>
          <Button
            size="small"
            icon={<PlusOutlined />}
            style={{ marginBottom: 12 }}
            onClick={() => setAdding(true)}
          >
            {t('prov.addEndpoint')}
          </Button>
          <Space direction="vertical" size="middle" style={{ width: '100%' }}>
            {list.map((c) => (
              <EndpointCard
                key={c.id}
                channel={c}
                onEdit={() => setEditing(c)}
                onChanged={onChanged}
              />
            ))}
            {list.length === 0 && (
              <Typography.Text type="secondary">{t('prov.addEndpoint')} →</Typography.Text>
            )}
          </Space>
        </>
      )}

      <EndpointModal
        open={adding || !!editing}
        editing={editing}
        providerId={provider?.id ?? 0}
        onClose={() => {
          setAdding(false);
          setEditing(null);
        }}
        onDone={() => {
          setAdding(false);
          setEditing(null);
          onChanged();
        }}
      />
    </Drawer>
  );
}

function EndpointCard({
  channel,
  onEdit,
  onChanged,
}: {
  channel: Channel;
  onEdit: () => void;
  onChanged: () => void;
}) {
  const { t } = useLang();
  const { message } = AntApp.useApp();
  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState<
    | { ok: boolean; mode: string; status: number; error?: string; dns_ms: number; connect_ms: number; tls_ms: number; first_byte_ms: number; total_ms: number; model: string; entries?: number }
    | null
  >(null);

  const toggle = async (enabled: boolean) => {
    await api.put(`/api/v1/channels/${channel.id}`, { enabled }).catch(() => message.error('failed'));
    onChanged();
  };

  const remove = async () => {
    await api.del(`/api/v1/channels/${channel.id}`).catch((e) => message.error(e.message));
    onChanged();
  };

  // Live connection probe: minimal 1-token request through this endpoint,
  // with DNS / TCP / TLS / first-byte / total timing breakdown.
  const runTest = async () => {
    setTesting(true);
    setTestResult(null);
    try {
      const r = await api.post<{
        ok: boolean; mode: string; status: number; error?: string; dns_ms: number;
        connect_ms: number; tls_ms: number; first_byte_ms: number; total_ms: number;
        model: string; entries?: number;
      }>(`/api/v1/channels/${channel.id}/test`);
      setTestResult(r);
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    } finally {
      setTesting(false);
    }
  };

  const timing = testResult && (
    <span>
      {testResult.first_byte_ms > 0 && (
        <>
          {t('prov.test.ttfb')} {testResult.first_byte_ms}ms ·{' '}
        </>
      )}
      {t('prov.test.total')} {testResult.total_ms}ms
      {testResult.connect_ms > 0 && (
        <>
          {' '}· {t('prov.test.connect')} {testResult.connect_ms}ms
          {testResult.tls_ms > 0 ? ` · ${t('prov.test.tls')} ${testResult.tls_ms}ms` : ''}
        </>
      )}
      {testResult.mode === 'chat' && testResult.model ? ` · ${testResult.model}` : ''}
      {testResult.mode === 'models_list' && (
        <>
          {' '}· {t('prov.test.modelsList')}
          {testResult.entries != null ? ` · ${testResult.entries} ${t('prov.test.models')}` : ''}
        </>
      )}
    </span>
  );

  return (
    <div style={{ border: '1px solid #f0f0f0', borderRadius: 8, padding: 12 }}>
      <Space style={{ width: '100%', justifyContent: 'space-between' }}>
        <Space>
          <Tag color={channel.protocol === 'openai' ? 'green' : 'orange'}>{channel.protocol}</Tag>
          <Typography.Text strong>{channel.name}</Typography.Text>
          <Typography.Text type="secondary" code>
            {channel.base_url}
            {channel.chat_path}
          </Typography.Text>
        </Space>
        <Space>
          <span>{t('common.enabled')}</span>
          <Switch size="small" checked={channel.enabled} onChange={toggle} />
          <Button size="small" loading={testing} onClick={runTest}>
            {testing ? t('prov.testing') : t('prov.test')}
          </Button>
          <Button size="small" onClick={onEdit}>{t('common.edit')}</Button>
          <Popconfirm title={t('common.delete') + '?'} onConfirm={remove}>
            <Button danger size="small">{t('common.delete')}</Button>
          </Popconfirm>
        </Space>
      </Space>
      <div style={{ marginTop: 8 }}>
        <Typography.Text type="secondary">
          {t('prov.priority')}: {channel.priority} · {t('prov.weight')}: {channel.weight} ·{' '}
        </Typography.Text>
        {(channel.models ?? []).map((m) => (
          <Tag key={m.model}>
            {m.model} → {m.upstream_model}
          </Tag>
        ))}
      </div>
      {testResult && (
        <div style={{ marginTop: 8 }}>
          {testResult.ok ? (
            <Typography.Text type="success">
              ✓ {t('prov.testOk')} · HTTP {testResult.status} · {timing}
            </Typography.Text>
          ) : (
            <Typography.Text type="danger">
              ✗ {testResult.status ? `HTTP ${testResult.status}` : ''}{' '}
              {testResult.error ?? ''}{' '}
              {testResult.total_ms > 0 && (
                <Typography.Text type="secondary">
                  ({t('prov.test.total')} {testResult.total_ms}ms)
                </Typography.Text>
              )}
            </Typography.Text>
          )}
        </div>
      )}
    </div>
  );
}

interface EndpointForm {
  name: string;
  presetKey?: string;
  protocol: 'openai' | 'anthropic';
  base_url: string;
  chat_path: string;
  auth_style: 'bearer' | 'x-api-key';
  priority: number;
  weight: number;
  enabled: boolean;
  passthrough: boolean;
  supports_embeddings: boolean;
  models: ChannelModel[];
}

function EndpointModal({
  open,
  editing,
  providerId,
  onClose,
  onDone,
}: {
  open: boolean;
  editing: Channel | null;
  providerId: number;
  onClose: () => void;
  onDone: () => void;
}) {
  const { t } = useLang();
  const { message } = AntApp.useApp();
  const [form] = Form.useForm<EndpointForm>();

  useEffect(() => {
    if (!open) return;
    if (editing) {
      form.setFieldsValue({ ...editing, models: editing.models ?? [] });
    } else {
      form.resetFields();
      form.setFieldsValue({
        protocol: 'openai',
        auth_style: 'bearer',
        priority: 10,
        weight: 1,
        enabled: true,
        passthrough: true,
        supports_embeddings: false,
        chat_path: '/chat/completions',
        models: [],
      });
    }
  }, [open, editing, form]);

  const applyPreset = (key?: string) => {
    const p = endpointPresets.find((x) => presetKey(x) === key) ?? null;
    if (p) {
      form.setFieldsValue({
        protocol: p.protocol,
        base_url: p.base_url,
        chat_path: p.chat_path,
        auth_style: p.auth_style,
      });
      // Auto-bind the vendor's known models (identity mapping) only when no
      // bindings exist yet, so editing never silently overwrites user edits.
      const current = form.getFieldValue('models') as ChannelModel[] | undefined;
      if ((!current || current.length === 0) && p.models.length > 0) {
        form.setFieldsValue({
          models: p.models.map((m) => ({ model: m, upstream_model: m })),
        });
      }
    }
  };

  const submit = async (v: EndpointForm) => {
    try {
      if (editing) {
        await api.put(`/api/v1/channels/${editing.id}`, { ...v, provider_id: providerId });
      } else {
        await api.post('/api/v1/channels', { ...v, provider_id: providerId });
      }
      message.success(t('prov.saved'));
      onDone();
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    }
  };

  return (
    <Modal
      title={editing ? `${t('prov.editEndpoint')} — ${editing.name}` : t('prov.addEndpoint')}
      open={open}
      onCancel={onClose}
      onOk={() => form.submit()}
      width={640}
      destroyOnClose
    >
      <Form form={form} layout="vertical" onFinish={submit}>
        <Form.Item name="presetKey" label={t('prov.preset')} tooltip={t('prov.presetTip')}>
          <Select
            allowClear
            placeholder={t('prov.presetCustom')}
            onChange={applyPreset}
            options={endpointPresets.map((p) => ({ value: presetKey(p), label: presetKey(p) }))}
          />
        </Form.Item>
        <Space style={{ width: '100%' }} size="large">
          <Form.Item name="name" label={t('common.name')} rules={[{ required: true }]}>
            <Input placeholder="deepseek-openai" style={{ width: 200 }} />
          </Form.Item>
          <Form.Item name="protocol" label={t('prov.protocol')} rules={[{ required: true }]}>
            <Select
              style={{ width: 180 }}
              options={[
                { value: 'openai', label: t('prov.openaiCompat') },
                { value: 'anthropic', label: t('prov.anthropicCompat') },
              ]}
            />
          </Form.Item>
          <Form.Item name="auth_style" label={t('prov.authStyle')}>
            <Select
              style={{ width: 140 }}
              options={[
                { value: 'bearer', label: t('prov.bearer') },
                { value: 'x-api-key', label: t('prov.xApiKey') },
              ]}
            />
          </Form.Item>
        </Space>
        <Form.Item name="base_url" label={t('prov.baseUrl')} rules={[{ required: true }]}>
          <Input placeholder="https://api.deepseek.com" />
        </Form.Item>
        <Form.Item name="chat_path" label={t('prov.chatPath')} tooltip={t('prov.chatPathTip')}>
          <Input placeholder="/chat/completions" />
        </Form.Item>
        <Space size="large">
          <Form.Item name="priority" label={t('prov.priority')} tooltip={t('prov.priorityTip')}>
            <InputNumber />
          </Form.Item>
          <Form.Item name="weight" label={t('prov.weight')}>
            <InputNumber min={1} />
          </Form.Item>
          <Form.Item name="enabled" label={t('common.enabled')} valuePropName="checked">
            <Switch />
          </Form.Item>
          <Form.Item name="passthrough" label="Passthrough" valuePropName="checked">
            <Switch />
          </Form.Item>
          <Form.Item name="supports_embeddings" label="Embeddings" valuePropName="checked">
            <Switch />
          </Form.Item>
        </Space>

        <Typography.Title level={5} style={{ marginTop: 8 }}>
          {t('prov.bindings')}
        </Typography.Title>
        <Form.List name="models">
          {(fields, { add, remove }) => (
            <div>
              {fields.map((field) => (
                <Space key={field.key} style={{ display: 'flex', marginBottom: 8 }} align="baseline">
                  <Form.Item name={[field.name, 'model']} noStyle>
                    <Input placeholder={t('prov.clientModel')} style={{ width: 220 }} />
                  </Form.Item>
                  <span>→</span>
                  <Form.Item name={[field.name, 'upstream_model']} noStyle>
                    <Input placeholder={t('prov.upstreamModel')} style={{ width: 220 }} />
                  </Form.Item>
                  <Button size="small" onClick={() => remove(field.name)}>
                    {t('common.remove')}
                  </Button>
                </Space>
              ))}
              <Button size="small" onClick={() => add()}>
                {t('common.add')}
              </Button>
            </div>
          )}
        </Form.List>
      </Form>
    </Modal>
  );
}

// — keys sub-table (shared key pool of the provider) ------------------------

function KeysTable({ providerId }: { providerId: number }) {
  const { t } = useLang();
  const { message } = AntApp.useApp();
  const [keys, setKeys] = useState<ProviderKey[]>([]);
  const [label, setLabel] = useState('');
  const [apiKey, setApiKey] = useState('');
  const [weight, setWeight] = useState(1);

  const load = useCallback(() => {
    api.get<ProviderKey[]>(`/api/v1/providers/${providerId}/keys`).then((x) => setKeys(x ?? [])).catch(() => {});
  }, [providerId]);
  useEffect(load, [load]);

  const add = async () => {
    if (!apiKey) return;
    try {
      await api.post(`/api/v1/providers/${providerId}/keys`, { label, api_key: apiKey, weight });
      setApiKey('');
      setLabel('');
      message.success(t('prov.keyAdded'));
      load();
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    }
  };

  return (
    <div>
      <Space style={{ marginBottom: 8 }} wrap>
        <Input placeholder={t('prov.keyLabel')} value={label} onChange={(e) => setLabel(e.target.value)} style={{ width: 120 }} />
        <Input.Password placeholder={t('prov.key')} value={apiKey} onChange={(e) => setApiKey(e.target.value)} style={{ width: 280 }} />
        <InputNumber min={1} value={weight} onChange={(v) => setWeight(v ?? 1)} />
        <Button onClick={add}>{t('prov.addKey')}</Button>
      </Space>
      <Table<ProviderKey>
        size="small"
        rowKey="id"
        dataSource={keys}
        columns={[
          { title: t('prov.keyLabel'), dataIndex: 'label' },
          { title: t('keys.keyColumn'), dataIndex: 'api_key_mask' },
          { title: t('prov.weight'), dataIndex: 'weight', width: 80 },
          {
            title: t('common.enabled'),
            dataIndex: 'enabled',
            render: (v: boolean, k) => (
              <Switch
                size="small"
                checked={v}
                onChange={async (x) => {
                  await api.put(`/api/v1/keys/${k.id}`, { enabled: x });
                  load();
                }}
              />
            ),
          },
          {
            title: '',
            render: (_, k) => (
              <Popconfirm
                title={t('common.delete') + '?'}
                onConfirm={async () => {
                  await api.del(`/api/v1/keys/${k.id}`);
                  load();
                }}
              >
                <Button danger size="small">{t('common.delete')}</Button>
              </Popconfirm>
            ),
          },
        ]}
        pagination={false}
      />
    </div>
  );
}
