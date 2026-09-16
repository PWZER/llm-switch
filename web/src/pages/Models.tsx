import { useCallback, useEffect, useMemo, useState } from 'react';
import {
  App as AntApp, Button, Form, Input, InputNumber, Modal, Popconfirm, Select, Space, Switch,
  Table, Tag, Typography,
} from 'antd';
import { PlusOutlined, SyncOutlined } from '@ant-design/icons';
import { api, Account, Channel, Model, Provider } from '../api/client';
import { useLang } from '../i18n/i18n';
import { ProviderLogo } from '../components/logo';

// Token counts render in compact K/M units (1024-base, the convention for
// context windows: 131072 -> 128K, 1048576 -> 1M). Display only — the raw
// value stays editable in the form.
const formatTokens = (v: number | null): string => {
  if (v == null) return '-';
  const trim = (n: number): string =>
    Number.isInteger(n) ? String(n) : n.toFixed(2).replace(/0+$/, '').replace(/\.$/, '');
  if (v >= 1024 * 1024) return `${trim(v / (1024 * 1024))}M`;
  if (v >= 1024) return `${trim(v / 1024)}K`;
  return String(v);
};

interface ModelFormValues {
  id: string;
  provider_ids: number[];
  upstream_model?: string;
  display_name?: string;
  context_window?: number;
  max_output_tokens?: number;
}

export default function Models() {
  const { t } = useLang();
  const { message } = AntApp.useApp();
  const [rows, setRows] = useState<Model[]>([]);
  const [providers, setProviders] = useState<Provider[]>([]);
  const [channels, setChannels] = useState<Channel[]>([]);
  const [open, setOpen] = useState(false);
  const [editing, setEditing] = useState<Model | null>(null);
  const [form] = Form.useForm<ModelFormValues>();
  const [editForm] = Form.useForm<Omit<ModelFormValues, 'provider_ids'>>();

  // Filters
  const [search, setSearch] = useState('');
  const [providerFilter, setProviderFilter] = useState<string | undefined>();

  const load = useCallback(() => {
    api.get<Model[]>('/api/v1/models').then((x) => setRows(x ?? [])).catch((e) => message.error(e.message));
    api.get<Provider[]>('/api/v1/providers').then((x) => setProviders(x ?? [])).catch(() => {});
    api.get<Channel[]>('/api/v1/channels').then((x) => setChannels(x ?? [])).catch(() => {});
  }, [message]);
  useEffect(load, [load]);

  // One row per selected provider; the alias defaults to identity per row.
  const create = async (v: ModelFormValues) => {
    try {
      await api.post('/api/v1/models', v);
      message.success(t('common.ok'));
      setOpen(false);
      form.resetFields();
      load();
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    }
  };

  const saveEdit = async (v: Omit<ModelFormValues, 'id' | 'provider_ids'>) => {
    if (!editing) return;
    try {
      await api.put(`/api/v1/models/${editing.provider_id}/${encodeURIComponent(editing.id)}`, v);
      message.success(t('common.ok'));
      setEditing(null);
      load();
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    }
  };

  const toggle = async (m: Model, enabled: boolean) => {
    await api
      .put(`/api/v1/models/${m.provider_id}/${encodeURIComponent(m.id)}`, { enabled })
      .catch(() => message.error('failed'));
    load();
  };

  // Fetch a provider's upstream model list. The account authenticating the
  // upstream call is chosen explicitly — there is no silent default.
  const [fetchOpen, setFetchOpen] = useState(false);
  const [fetching, setFetching] = useState(false);
  const [fetchProvider, setFetchProvider] = useState<number | undefined>();
  const [fetchAccounts, setFetchAccounts] = useState<Account[]>([]);
  const [fetchAccount, setFetchAccount] = useState<number | undefined>();

  useEffect(() => {
    if (fetchProvider == null) {
      setFetchAccounts([]);
      setFetchAccount(undefined);
      return;
    }
    api.get<Account[]>(`/api/v1/providers/${fetchProvider}/accounts`)
      .then((list) => {
        const enabled = (list ?? []).filter((a) => a.enabled);
        setFetchAccounts(list ?? []);
        setFetchAccount(enabled[0]?.id);
      })
      .catch(() => {});
  }, [fetchProvider]);

  const refreshUpstream = async () => {
    if (fetchProvider == null || fetchAccount == null) return;
    setFetching(true);
    try {
      const r = await api.post<{ models_added: number }>(
        `/api/v1/providers/${fetchProvider}/refresh-models`,
        { account_id: fetchAccount },
      );
      message.success(`${t('models.fetched')} (${r.models_added} ${t('models.new')})`);
      setFetchOpen(false);
      load();
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    } finally {
      setFetching(false);
    }
  };

  // Enabled first, then name; fuzzy name search + provider filter applied
  // client-side.
  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase();
    return rows
      .filter((m) => {
        if (q && !m.id.toLowerCase().includes(q) && !m.display_name.toLowerCase().includes(q)) {
          return false;
        }
        if (providerFilter && m.provider_name !== providerFilter) {
          return false;
        }
        return true;
      })
      .sort((a, b) => {
        if (a.enabled !== b.enabled) return a.enabled ? -1 : 1;
        return a.id.localeCompare(b.id) || a.provider_id - b.provider_id;
      });
  }, [rows, search, providerFilter]);

  const providerFilterOptions = useMemo(() => {
    const names = new Set<string>();
    rows.forEach((m) => m.provider_name && names.add(m.provider_name));
    return Array.from(names).sort().map((p) => ({ value: p, label: p }));
  }, [rows]);

  const providerOptions = useMemo(
    () => providers.map((p) => ({ value: p.id, label: p.name })),
    [providers],
  );

  // The protocols column is derived: a row's provider serves the model
  // through every live channel it owns, so the column renders the distinct
  // protocols of those channels; a protocol present only on disabled
  // channels is struck through.
  const protocolsByProvider = useMemo(() => {
    const map = new Map<number, { name: string; live: boolean }[]>();
    channels.forEach((c) => {
      const list = map.get(c.provider_id) ?? [];
      const p = list.find((x) => x.name === c.protocol);
      if (p) p.live = p.live || c.enabled;
      else list.push({ name: c.protocol, live: c.enabled });
      map.set(c.provider_id, list);
    });
    return map;
  }, [channels]);

  return (
    <div>
      <Space style={{ marginBottom: 16 }} wrap>
        <Button type="primary" icon={<PlusOutlined />} onClick={() => setOpen(true)}>
          {t('models.add')}
        </Button>
        <Button icon={<SyncOutlined />} onClick={() => setFetchOpen(true)}>
          {t('models.fetch')}
        </Button>
        <Button onClick={load}>{t('common.refresh')}</Button>
      </Space>
      <Space style={{ marginBottom: 16 }} wrap>
        <Input
          placeholder={t('models.searchPlaceholder')}
          style={{ width: 220 }}
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          allowClear
          prefix={<SearchIcon />}
        />
        <Select
          placeholder={t('models.allProviders')}
          style={{ width: 180 }}
          allowClear
          value={providerFilter}
          onChange={(v) => setProviderFilter(v)}
          options={providerFilterOptions}
        />
        <Typography.Text type="secondary">
          {filtered.filter((m) => m.enabled).length} / {rows.length} {t('models.enabledCount')}
        </Typography.Text>
      </Space>
      <Table<Model>
        rowKey={(m) => `${m.provider_id}|${m.id}`}
        dataSource={filtered}
        columns={[
          { title: t('dash.model'), dataIndex: 'id' },
          { title: t('models.displayName'), dataIndex: 'display_name' },
          {
            title: t('models.source'),
            render: (_, m) =>
              m.provider_name ? (
                <Space size={6}>
                  <ProviderLogo name={m.provider_name} size={18} />
                  <span>{m.provider_name}</span>
                  {m.provider_enabled === false && <Tag>{t('models.providerDisabled')}</Tag>}
                </Space>
              ) : (
                <Tag>-</Tag>
              ),
          },
          {
            title: t('models.protocols'),
            render: (_, m) => {
              const protocols = protocolsByProvider.get(m.provider_id) ?? [];
              if (protocols.length === 0) return <Tag>-</Tag>;
              return (
                <Space size={4} wrap>
                  {protocols.map((p) => (
                    <Tag
                      key={p.name}
                      color={p.live ? (p.name === 'openai' ? 'green' : 'orange') : undefined}
                      style={p.live ? undefined : { textDecoration: 'line-through', opacity: 0.6 }}
                    >
                      {p.name}
                    </Tag>
                  ))}
                </Space>
              );
            },
          },
          {
            title: t('models.upstream'),
            dataIndex: 'upstream_model',
            render: (v: string, m) => v || m.id,
          },
          {
            title: t('models.context'),
            dataIndex: 'context_window',
            render: formatTokens,
          },
          {
            title: t('models.maxOutput'),
            dataIndex: 'max_output_tokens',
            render: formatTokens,
          },
          {
            title: t('common.enabled'),
            dataIndex: 'enabled',
            render: (v: boolean, m) => (
              <Switch size="small" checked={v} onChange={(x) => toggle(m, x)} />
            ),
          },
          {
            title: '',
            render: (_, m) => (
              <Space>
                <Button
                  size="small"
                  onClick={() => {
                    setEditing(m);
                    editForm.setFieldsValue({
                      id: m.id,
                      upstream_model: m.upstream_model,
                      display_name: m.display_name,
                      context_window: m.context_window ?? undefined,
                      max_output_tokens: m.max_output_tokens ?? undefined,
                    });
                  }}
                >
                  {t('common.edit')}
                </Button>
                <Popconfirm
                  title={t('models.deleteConfirm')}
                  onConfirm={async () => {
                    await api.del(`/api/v1/models/${m.provider_id}/${encodeURIComponent(m.id)}`);
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
      <Modal
        title={t('models.fetch')}
        open={fetchOpen}
        onCancel={() => setFetchOpen(false)}
        onOk={refreshUpstream}
        okButtonProps={{ loading: fetching, disabled: fetchProvider == null || fetchAccount == null }}
        destroyOnClose
      >
        <Form layout="vertical">
          <Form.Item label={t('acct.provider')} required>
            <Select
              value={fetchProvider}
              onChange={setFetchProvider}
              options={providers.map((p) => ({ value: p.id, label: p.name }))}
              placeholder={t('acct.provider')}
            />
          </Form.Item>
          <Form.Item label={t('models.fetchAccount')} required tooltip={t('models.fetchAccountTip')}>
            <Select
              value={fetchAccount}
              onChange={setFetchAccount}
              options={fetchAccounts.map((a) => ({
                value: a.id,
                label: `${a.label || a.api_key_mask} (${a.api_key_mask})${a.enabled ? '' : ' (off)'}`,
                disabled: !a.enabled,
              }))}
              placeholder={t('models.fetchAccount')}
              notFoundContent={t('acct.noAccounts')}
            />
          </Form.Item>
        </Form>
      </Modal>
      <Modal
        title={t('models.add')}
        open={open}
        onCancel={() => setOpen(false)}
        onOk={() => form.submit()}
        destroyOnClose
      >
        <Form form={form} layout="vertical" onFinish={create}>
          <Form.Item name="id" label={t('models.modelId')} rules={[{ required: true }]}>
            <Input placeholder={t('models.displayPlaceholder')} />
          </Form.Item>
          <Form.Item
            name="provider_ids"
            label={t('models.source')}
            rules={[{ required: true }]}
            tooltip={t('models.providersTip')}
          >
            <Select mode="multiple" options={providerOptions} placeholder={t('models.providersTip')} />
          </Form.Item>
          <Form.Item name="upstream_model" label={t('models.upstream')} tooltip={t('models.upstreamTip')}>
            <Input placeholder={t('models.upstreamTip')} />
          </Form.Item>
          <Form.Item name="display_name" label={t('models.displayName')}>
            <Input />
          </Form.Item>
          <Form.Item name="context_window" label={t('models.context')}>
            <InputNumber style={{ width: '100%' }} min={0} />
          </Form.Item>
          <Form.Item name="max_output_tokens" label={t('models.maxOutput')}>
            <InputNumber style={{ width: '100%' }} min={0} />
          </Form.Item>
        </Form>
      </Modal>
      <Modal
        title={`${t('common.edit')}: ${editing?.id ?? ''}`}
        open={editing != null}
        onCancel={() => setEditing(null)}
        onOk={() => editForm.submit()}
        destroyOnClose
      >
        <Form form={editForm} layout="vertical" onFinish={saveEdit}>
          <Form.Item name="id" label={t('models.modelId')}>
            <Input disabled />
          </Form.Item>
          <Form.Item name="upstream_model" label={t('models.upstream')} tooltip={t('models.upstreamTip')}>
            <Input placeholder={t('models.upstreamTip')} />
          </Form.Item>
          <Form.Item name="display_name" label={t('models.displayName')}>
            <Input />
          </Form.Item>
          <Form.Item name="context_window" label={t('models.context')}>
            <InputNumber style={{ width: '100%' }} min={0} />
          </Form.Item>
          <Form.Item name="max_output_tokens" label={t('models.maxOutput')}>
            <InputNumber style={{ width: '100%' }} min={0} />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  );
}

function SearchIcon() {
  // Minimal inline magnifier to avoid another icon import surface.
  return (
    <span role="img" aria-label="search" style={{ color: '#bbb' }}>
      🔍
    </span>
  );
}
