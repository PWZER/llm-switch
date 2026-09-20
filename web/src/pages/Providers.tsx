import { useCallback, useEffect, useState, type ReactNode } from 'react';
import {
  App as AntApp, Button, Drawer, Form, Input, InputNumber, Modal, Popconfirm,
  Select, Space, Switch, Table, Typography, theme,
} from 'antd';
import { PlusOutlined, SettingOutlined, SyncOutlined, DownOutlined } from '@ant-design/icons';
import {
  api, Channel, Model, Provider,
} from '../api/client';
import { useLang } from '../i18n/i18n';
import { AccountPickerModal } from '../components/account';
import { ProviderLogo } from '../components/logo';
import ModelSelectList, { ModelSelectItem } from '../components/ModelSelectList';
import { ProtocolTag, protocolOrder } from '../components/protocol';

// Merged provider management: one provider (vendor account + endpoints) hosts
// one or more protocol endpoints, each with its own failover priority. The
// provider also owns the models-list fetch URL (models_url, absolute) and the
// staged register_models list — model rows are provider-scoped and register
// at provider save time. Credentials live on accounts, managed on the Account
// Pool page — this drawer handles names, models_url and endpoints only. A
// single ProviderDrawer serves both create (endpoints buffer locally,
// persisted in one submit) and manage (every section mutates the API
// immediately).

export default function Providers() {
  const { t } = useLang();
  const { message } = AntApp.useApp();
  const [providers, setProviders] = useState<Provider[]>([]);
  const [channels, setChannels] = useState<Channel[]>([]);
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [editing, setEditing] = useState<Provider | null>(null);

  const load = useCallback(() => {
    api
      .get<Provider[]>('/api/v1/providers')
      .then((x) => {
        const list = x ?? [];
        setProviders(list);
        // Keep the open drawer pointed at fresh data after remote mutations.
        setEditing((prev) => (prev ? (list.find((p) => p.id === prev.id) ?? prev) : prev));
      })
      .catch((e) => message.error(e.message));
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
          {t('prov.new')}
        </Button>
        <Button onClick={load}>{t('common.refresh')}</Button>
      </Space>
      <Table<Provider>
        rowKey="id"
        dataSource={providers}
        columns={[
          {
            title: t('common.name'),
            dataIndex: 'name',
            render: (v: string) => (
              <Space size={8}>
                <ProviderLogo name={v} size={20} />
                <span>{v}</span>
              </Space>
            ),
          },
          {
            title: t('common.enabled'),
            dataIndex: 'enabled',
            render: (v: boolean, p) => <Switch checked={v} onChange={(x) => toggle(p, x)} />,
          },
          {
            title: t('prov.endpoints'),
            render: (_, p) => {
              // Compact overview chips in canonical protocol order; the manage
              // drawer keeps API/priority order (meaningful for failover).
              const list = channels
                .filter((c) => c.provider_id === p.id)
                .sort((a, b) => protocolOrder(a.protocol) - protocolOrder(b.protocol));
              if (list.length === 0) return <Typography.Text type="secondary">-</Typography.Text>;
              return (
                <Space wrap>
                  {list.map((c) => (
                    <Tooltiped key={c.id} title={`${c.base_url}${c.chat_path}`}>
                      <ProtocolTag protocol={c.protocol}>
                        {c.protocol}
                        {!c.enabled ? ' (off)' : ''}
                      </ProtocolTag>
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
                <Button
                  size="small"
                  icon={<SettingOutlined />}
                  onClick={() => {
                    setEditing(p);
                    setDrawerOpen(true);
                  }}
                >
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

      <ProviderDrawer
        open={drawerOpen}
        provider={editing}
        channels={channels}
        onClose={() => setDrawerOpen(false)}
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

// — provider drawer: create (local buffering) + manage (remote) --------------

function ProviderDrawer({
  open,
  provider,
  channels,
  onClose,
  onChanged,
}: {
  open: boolean;
  provider: Provider | null;
  channels: Channel[];
  onClose: () => void;
  onChanged: () => void;
}) {
  const { t } = useLang();
  const { message } = AntApp.useApp();
  const isCreate = provider == null;

  // Create-mode local buffers.
  const [name, setName] = useState('');
  const [modelsUrl, setModelsUrl] = useState('');
  const [stagedModels, setStagedModels] = useState<string[]>([]);
  const [localEndpoints, setLocalEndpoints] = useState<EndpointForm[]>([]);
  const [creating, setCreating] = useState(false);

  // Fetched-preview selection state (both modes): the fetched ids are shown
  // for per-model choice; registeredIds marks rows this provider already has
  // (rendered checked + locked, never re-staged).
  const [previewItems, setPreviewItems] = useState<ModelSelectItem[]>([]);
  const [registeredIds, setRegisteredIds] = useState<Set<string>>(new Set());
  const [savingModels, setSavingModels] = useState(false);
  // The staged ids that would actually register: registered rows are locked
  // in the list and excluded from register_models.
  const stagedCount = stagedModels.filter((id) => !registeredIds.has(id)).length;

  // Manage-mode buffers.
  const [editName, setEditName] = useState('');
  const [editModelsUrl, setEditModelsUrl] = useState('');

  // Fetch-models flow (both modes): the credential is picked explicitly.
  const [fetching, setFetching] = useState(false);
  const [pickerOpen, setPickerOpen] = useState(false);

  useEffect(() => {
    if (!open) return;
    setName('');
    setModelsUrl('');
    setStagedModels([]);
    setPreviewItems([]);
    setRegisteredIds(new Set());
    setLocalEndpoints([]);
    setEditName(provider?.name ?? '');
    setEditModelsUrl(provider?.models_url ?? '');
  }, [open, provider?.id]); // eslint-disable-line react-hooks/exhaustive-deps

  const currentModelsUrl = isCreate ? modelsUrl : editModelsUrl;
  const setCurrentModelsUrl = isCreate ? setModelsUrl : setEditModelsUrl;

  // The preview's auth header style follows the provider's own endpoints
  // (openai first — the models list is an OpenAI-style endpoint), mirroring
  // the backend refresh-models resolution.
  const providerAuthStyle = (() => {
    const styles = isCreate
      ? localEndpoints.map((e) => ({ protocol: e.protocol, auth_style: e.auth_style }))
      : channels
          .filter((c) => c.provider_id === provider?.id && c.enabled)
          .map((c) => ({ protocol: c.protocol, auth_style: c.auth_style }));
    return (styles.find((s) => s.protocol === 'openai') ?? styles[0])?.auth_style ?? 'bearer';
  })();

  const fetchModelsPreview = async (cred: { account_id?: number; api_key?: string }) => {
    if (!currentModelsUrl.trim()) {
      message.warning(t('prov.modelsUrl') + ' ?');
      return;
    }
    setFetching(true);
    try {
      const r = await api.post<{ models: string[] }>(
        `/api/v1/providers/${provider?.id ?? 0}/models-preview`,
        { models_url: currentModelsUrl.trim(), auth_style: providerAuthStyle, ...cred },
      );
      // Nothing is staged automatically: the fetched list is presented for
      // selection; only the checked (and not-yet-registered) ids register at
      // save time.
      setStagedModels([]);
      let regSet = new Set<string>();
      if (provider) {
        const rows = await api.get<Model[]>('/api/v1/models');
        regSet = new Set((rows ?? []).filter((m) => m.provider_id === provider.id).map((m) => m.id));
      }
      setRegisteredIds(regSet);
      setPreviewItems(r.models.map((id) => ({ id, registered: regSet.has(id), stale: false })));
      message.success(`${r.models.length} ${t('prov.fetchedPreview')}`);
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    } finally {
      setFetching(false);
    }
  };

  const rename = async () => {
    if (!provider || !editName.trim() || editName.trim() === provider.name) return;
    try {
      await api.put(`/api/v1/providers/${provider.id}`, { name: editName.trim() });
      message.success(t('prov.saved'));
      onChanged();
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    }
  };

  // Manage mode: persist models_url (empty string clears) and register the
  // checked model ids in one provider update (registered rows are locked in
  // the list and never re-staged).
  const saveModels = async () => {
    if (!provider) return;
    setSavingModels(true);
    const toRegister = stagedModels.filter((id) => !registeredIds.has(id));
    try {
      await api.put(`/api/v1/providers/${provider.id}`, {
        models_url: editModelsUrl.trim(),
        register_models: toRegister.length > 0 ? toRegister : undefined,
      });
      setStagedModels([]);
      setPreviewItems([]);
      message.success(t('prov.saved'));
      onChanged();
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    } finally {
      setSavingModels(false);
    }
  };

  const submitCreate = async () => {
    if (!name.trim()) {
      message.warning(t('common.name') + ' ?');
      return;
    }
    setCreating(true);
    try {
      const created = await api.post<{ id: number }>('/api/v1/providers', {
        name: name.trim(),
        models_url: modelsUrl.trim() || undefined,
        register_models: stagedCount > 0 ? stagedModels.filter((id) => !registeredIds.has(id)) : undefined,
      });
      let partial = false;
      for (const ep of localEndpoints) {
        try {
          await api.post('/api/v1/channels', { ...ep, provider_id: created.id });
        } catch {
          partial = true;
        }
      }
      if (partial) message.warning(t('prov.createPartial'));
      else message.success(t('prov.created'));
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
      title={
        isCreate ? (
          t('prov.new')
        ) : (
          <Space size={8}>
            <ProviderLogo name={provider.name} size={20} />
            <span>{`${t('prov.manage')} — ${provider.name}`}</span>
          </Space>
        )
      }
      open={open}
      onClose={onClose}
      width={960}
    >
      {open && (
        <>
          <Typography.Title level={5}>{t('common.name')}</Typography.Title>
          {isCreate ? (
            <Input
              placeholder={t('prov.namePlaceholder')}
              value={name}
              onChange={(e) => setName(e.target.value)}
              style={{ width: 280 }}
            />
          ) : (
            <Space>
              <Input
                value={editName}
                onChange={(e) => setEditName(e.target.value)}
                style={{ width: 280 }}
              />
              <Button
                size="small"
                disabled={!editName.trim() || editName.trim() === provider.name}
                onClick={rename}
              >
                {t('common.save')}
              </Button>
            </Space>
          )}

          <Typography.Title level={5} style={{ marginTop: 24 }}>
            {t('prov.modelsUrl')}
          </Typography.Title>
          <Space wrap>
            <Input
              placeholder="https://api.deepseek.com/models"
              value={currentModelsUrl}
              onChange={(e) => setCurrentModelsUrl(e.target.value)}
              style={{ width: 400 }}
            />
            {!isCreate && (
              <Button
                size="small"
                loading={savingModels}
                disabled={
                  editModelsUrl.trim() === (provider.models_url ?? '') && stagedCount === 0
                }
                onClick={saveModels}
              >
                {t('common.save')}
              </Button>
            )}
            <Button icon={<DownOutlined />} loading={fetching} onClick={() => setPickerOpen(true)}>
              {fetching ? t('prov.fetching') : t('prov.fetchModelsBtn')}
            </Button>
          </Space>
          <Typography.Paragraph type="secondary" style={{ fontSize: 12, marginTop: 4 }}>
            {t('prov.modelsUrlTip')}
          </Typography.Paragraph>
          {previewItems.length > 0 && (
            <>
              <Typography.Paragraph type="secondary" style={{ fontSize: 12 }}>
                {t('prov.registerModelsTip')}
              </Typography.Paragraph>
              <ModelSelectList
                items={previewItems}
                value={stagedModels}
                onChange={setStagedModels}
                disableRegistered
              />
            </>
          )}

          <Typography.Title level={5} style={{ marginTop: 24 }}>
            {t('prov.endpoints')}
          </Typography.Title>
          {isCreate ? (
            <LocalEndpoints
              value={localEndpoints}
              onChange={setLocalEndpoints}
            />
          ) : (
            <RemoteEndpoints
              providerId={provider.id}
              channels={channels}
              onChanged={onChanged}
            />
          )}
          <AccountPickerModal
            open={pickerOpen}
            providerId={provider?.id ?? 0}
            onClose={() => setPickerOpen(false)}
            onPick={(cred) => {
              setPickerOpen(false);
              fetchModelsPreview(cred);
            }}
          />
          {isCreate && (
            <div style={{ marginTop: 24 }}>
              <Button type="primary" loading={creating} onClick={submitCreate}>
                {t('common.create')}
              </Button>
            </div>
          )}
        </>
      )}
    </Drawer>
  );
}

// — endpoints section: remote (cards + test) or local (buffered list) --------

function RemoteEndpoints({
  providerId,
  channels,
  onChanged,
}: {
  providerId: number;
  channels: Channel[];
  onChanged: () => void;
}) {
  const { t } = useLang();
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<Channel | null>(null);

  const list = channels.filter((c) => c.provider_id === providerId);

  return (
    <>
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
      <EndpointModal
        open={adding || !!editing}
        editing={editing}
        providerId={providerId}
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
    </>
  );
}

function LocalEndpoints({
  value,
  onChange,
}: {
  value: EndpointForm[];
  onChange: (eps: EndpointForm[]) => void;
}) {
  const { t } = useLang();
  const [modalOpen, setModalOpen] = useState(false);
  const [editIdx, setEditIdx] = useState<number | null>(null);

  const openAdd = () => {
    setEditIdx(null);
    setModalOpen(true);
  };

  return (
    <>
      <Button
        size="small"
        icon={<PlusOutlined />}
        style={{ marginBottom: 12 }}
        onClick={openAdd}
      >
        {t('prov.addEndpoint')}
      </Button>
      <Space direction="vertical" size="middle" style={{ width: '100%' }}>
        {value.map((ep, i) => (
          <LocalEndpointCard
            key={i}
            endpoint={ep}
            onEdit={() => {
              setEditIdx(i);
              setModalOpen(true);
            }}
            onDelete={() => onChange(value.filter((_, j) => j !== i))}
          />
        ))}
        {value.length === 0 && (
          <Typography.Text type="secondary">{t('prov.addEndpoint')} →</Typography.Text>
        )}
      </Space>
      <EndpointModal
        open={modalOpen}
        editing={null}
        initial={editIdx != null ? value[editIdx] : null}
        providerId={0}
        onLocalSubmit={(v) => {
          onChange(editIdx != null ? value.map((x, j) => (j === editIdx ? v : x)) : [...value, v]);
        }}
        onClose={() => setModalOpen(false)}
        onDone={() => setModalOpen(false)}
      />
    </>
  );
}

function LocalEndpointCard({
  endpoint,
  onEdit,
  onDelete,
}: {
  endpoint: EndpointForm;
  onEdit: () => void;
  onDelete: () => void;
}) {
  const { t } = useLang();
  const { token } = theme.useToken();
  return (
    <div style={{ border: `1px solid ${token.colorBorderSecondary}`, borderRadius: 8, padding: 12 }}>
      <Space style={{ width: '100%', justifyContent: 'space-between' }}>
        <Space>
          <ProtocolTag protocol={endpoint.protocol} />
          <Typography.Text strong>{endpoint.name}</Typography.Text>
          <Typography.Text type="secondary" code>
            {endpoint.base_url}
            {endpoint.chat_path}
          </Typography.Text>
        </Space>
        <Space>
          <Button size="small" onClick={onEdit}>{t('common.edit')}</Button>
          <Popconfirm title={t('common.delete') + '?'} onConfirm={onDelete}>
            <Button danger size="small">{t('common.delete')}</Button>
          </Popconfirm>
        </Space>
      </Space>
      <div style={{ marginTop: 8 }}>
        <Typography.Text type="secondary">
          {t('prov.priority')}: {endpoint.priority} · {t('prov.weight')}: {endpoint.weight}
        </Typography.Text>
      </div>
    </div>
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
  const { token } = theme.useToken();
  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState<
    | { ok: boolean; class: string; status: number; error?: string; dns_ms: number; connect_ms: number; tls_ms: number; first_byte_ms: number; total_ms: number; model: string }
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
        ok: boolean; class: string; status: number; error?: string;
        dns_ms: number; connect_ms: number; tls_ms: number; first_byte_ms: number;
        total_ms: number; model: string;
      }>(`/api/v1/channels/${channel.id}/test`);
      setTestResult(r);
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    } finally {
      setTesting(false);
    }
  };

  // Verdict color by class: green = reachable (2xx, API-alive validation, or
  // auth unchecked — key validation is the account test's job); orange =
  // reachable with a fixable problem; red = unreachable.
  const verdict = testResult && (() => {
    switch (testResult.class) {
      case 'ok':
        return <Typography.Text type="success">✓ {t('prov.testOk')} · HTTP {testResult.status}</Typography.Text>;
      case 'validation':
        return <Typography.Text type="success">✓ {t('prov.test.class.validation')}</Typography.Text>;
      case 'auth':
        return <Typography.Text type="success">✓ {t('prov.test.class.authUnchecked')}</Typography.Text>;
      case 'unreachable':
        return <Typography.Text type="danger">✗ {t('prov.test.class.unreachable')}</Typography.Text>;
      default:
        return <Typography.Text type="warning">⚠ {t(`prov.test.class.${testResult.class}`)}</Typography.Text>;
    }
  })();

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
      {testResult.model ? ` · ${testResult.model}` : ''}
    </span>
  );

  return (
    <div style={{ border: `1px solid ${token.colorBorderSecondary}`, borderRadius: 8, padding: 12 }}>
      <Space style={{ width: '100%', justifyContent: 'space-between' }}>
        <Space>
          <ProtocolTag protocol={channel.protocol} />
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
          {t('prov.priority')}: {channel.priority} · {t('prov.weight')}: {channel.weight}
        </Typography.Text>
      </div>
      {testResult && (
        <div style={{ marginTop: 8 }}>
          <div>
            {verdict}
            {' · '}
            <Typography.Text type="secondary">{timing}</Typography.Text>
          </div>
          {testResult.error && (
            <Typography.Text type="secondary" style={{ fontSize: 12, wordBreak: 'break-all' }}>
              {testResult.error}
            </Typography.Text>
          )}
        </div>
      )}
    </div>
  );
}

interface EndpointForm {
  name: string;
  protocol: 'openai' | 'anthropic';
  base_url: string;
  chat_path: string;
  auth_style: 'bearer' | 'x-api-key';
  responses_path?: string;
  priority: number;
  weight: number;
  enabled: boolean;
  passthrough: boolean;
  supports_embeddings: boolean;
}

function EndpointModal({
  open,
  editing,
  initial,
  providerId,
  onLocalSubmit,
  onClose,
  onDone,
}: {
  open: boolean;
  editing: Channel | null;
  /** Local-mode edit initial values (provider not persisted yet). */
  initial?: EndpointForm | null;
  providerId: number;
  /** Local mode: hand the form values to the parent (provider not persisted). */
  onLocalSubmit?: (v: EndpointForm) => void;
  onClose: () => void;
  onDone: () => void;
}) {
  const { t } = useLang();
  const { message } = AntApp.useApp();
  const [form] = Form.useForm<EndpointForm>();
  // responses_path only applies to openai channels (backend rejects it for
  // anthropic), so the field is hidden and stripped for other protocols.
  const protocol = Form.useWatch('protocol', form);
  const [probing, setProbing] = useState(false);
  const [probeResult, setProbeResult] = useState<
    | { ok: boolean; class: string; status: number; error?: string; first_byte_ms: number; total_ms: number; model: string }
    | null
  >(null);

  // Connectivity probe of the form's CURRENT values (pre-save): no credential.
  const runProbe = async () => {
    const v = form.getFieldsValue();
    if (!v.base_url) {
      message.warning(t('prov.baseUrl') + ' ?');
      return;
    }
    setProbing(true);
    setProbeResult(null);
    try {
      const r = await api.post<{
        ok: boolean; class: string; status: number; error?: string;
        first_byte_ms: number; total_ms: number; model: string;
      }>(`/api/v1/providers/${providerId}/probe`, {
        protocol: v.protocol,
        base_url: v.base_url,
        chat_path: v.chat_path,
        auth_style: v.auth_style,
      });
      setProbeResult(r);
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    } finally {
      setProbing(false);
    }
  };

  const probeVerdict = probeResult && (() => {
    switch (probeResult.class) {
      case 'ok':
        return <Typography.Text type="success">✓ {t('prov.testOk')} · HTTP {probeResult.status} · {probeResult.first_byte_ms}ms/{probeResult.total_ms}ms</Typography.Text>;
      case 'validation':
        return <Typography.Text type="success">✓ {t('prov.test.class.validation')}</Typography.Text>;
      case 'auth':
        // 401/403 without a credential = reachable; key validation is the
        // account test's job (same verdict as the saved-channel test).
        return <Typography.Text type="success">✓ {t('prov.test.class.authUnchecked')}</Typography.Text>;
      case 'unreachable':
        return <Typography.Text type="danger">✗ {t('prov.test.class.unreachable')}</Typography.Text>;
      default:
        return <Typography.Text type="warning">⚠ {t(`prov.test.class.${probeResult.class}`)}</Typography.Text>;
    }
  })();

  useEffect(() => {
    if (!open) return;
    setProbeResult(null);
    if (editing) {
      form.setFieldsValue({
        name: editing.name,
        protocol: editing.protocol,
        base_url: editing.base_url,
        chat_path: editing.chat_path,
        auth_style: editing.auth_style,
        responses_path: editing.responses_path ?? undefined,
        priority: editing.priority,
        weight: editing.weight,
        enabled: editing.enabled,
        passthrough: editing.passthrough,
        supports_embeddings: editing.supports_embeddings,
      });
    } else if (initial) {
      form.resetFields();
      form.setFieldsValue(initial);
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
      });
    }
  }, [open, editing, initial, form]);

  const submit = async (v: EndpointForm) => {
    // Drop a stale responses_path left over from a protocol switch — the
    // backend rejects it on anthropic channels.
    if (v.protocol !== 'openai') v.responses_path = undefined;
    if (onLocalSubmit) {
      onLocalSubmit(v);
      onDone();
      return;
    }
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

  const editName = editing?.name ?? initial?.name;

  return (
    <Modal
      title={editName ? `${t('prov.editEndpoint')} — ${editName}` : t('prov.addEndpoint')}
      open={open}
      onCancel={onClose}
      onOk={() => form.submit()}
      width={640}
      destroyOnClose
    >
      <Form form={form} layout="vertical" onFinish={submit}>
        <Space style={{ width: '100%' }} size="large">
          <Form.Item name="name" label={t('common.name')} rules={[{ required: true }]}>
            <Input placeholder="deepseek-openai" style={{ width: 200 }} />
          </Form.Item>
          <Form.Item name="protocol" label={t('prov.protocol')} rules={[{ required: true }]}>
            <Select
              style={{ width: 180 }}
              options={[
                { value: 'anthropic', label: t('prov.anthropicCompat') },
                { value: 'openai', label: t('prov.openaiCompat') },
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
        {protocol === 'openai' && (
          <Form.Item name="responses_path" label={t('prov.responsesPath')} tooltip={t('prov.responsesPathTip')}>
            <Input placeholder="/responses" />
          </Form.Item>
        )}
        {!onLocalSubmit && (
          <Space style={{ marginBottom: 12 }} wrap>
            <Button icon={<SyncOutlined spin={probing} />} onClick={runProbe} disabled={probing}>
              {probing ? t('prov.testing') : t('prov.testConnection')}
            </Button>
          </Space>
        )}
        {probeVerdict && (
          <div style={{ marginBottom: 12 }}>
            {probeVerdict}
            {probeResult?.error && (
              <Typography.Text type="secondary" style={{ fontSize: 12, wordBreak: 'break-all', display: 'block' }}>
                {probeResult.error}
              </Typography.Text>
            )}
          </div>
        )}
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
      </Form>
    </Modal>
  );
}

