// Shared account components: the account pool table, create/edit modal,
// two-tier availability test, usage/balance probes and result rendering.
// Used by the Accounts page (global pool) and the provider drawer (scoped).

import { useCallback, useEffect, useState, type ReactNode } from 'react';
import {
  App as AntApp, Button, Collapse, Dropdown, Form, Input, InputNumber, Modal, Popconfirm,
  Progress, Select, Space, Switch, Table, Tag, Typography,
} from 'antd';
import { PlusOutlined, SyncOutlined } from '@ant-design/icons';
import {
  api, Account, AccountTestResult, AccountUsageReport, ProbeResult, Provider,
  UsageBalance, UsageProbe, UsageWindow,
} from '../api/client';
import { useLang } from '../i18n/i18n';
import { ProviderLogo } from './logo';

export const maskKey = (k: string) => (k.length > 8 ? `${k.slice(0, 4)}…${k.slice(-4)}` : '****');

// — accounts table (account pool) ---------------------------------------------

export function AccountsTable({ providers, onRefresh }: { providers: Provider[]; onRefresh?: () => void }) {
  const { t } = useLang();
  const { message } = AntApp.useApp();
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [modalOpen, setModalOpen] = useState(false);
  const [editing, setEditing] = useState<Account | null>(null);
  const [usageFor, setUsageFor] = useState<Account | null>(null);

  const load = useCallback(() => {
    api.get<Account[]>('/api/v1/accounts').then((x) => setAccounts(x ?? [])).catch(() => {});
  }, []);
  useEffect(load, [load]);

  const providerName = (id: number) =>
    providers.find((p) => p.id === id)?.name ?? `#${id}`;

  return (
    <div>
      <Space style={{ marginBottom: 16 }}>
        <Button
          type="primary"
          icon={<PlusOutlined />}
          onClick={() => {
            setEditing(null);
            setModalOpen(true);
          }}
        >
          {t('acct.add')}
        </Button>
        <Button
          onClick={() => {
            load();
            onRefresh?.();
          }}
        >
          {t('common.refresh')}
        </Button>
      </Space>
      <Table<Account>
        rowKey="id"
        dataSource={accounts}
        columns={[
          {
            title: t('acct.provider'),
            dataIndex: 'provider_name',
            render: (v: string, a: Account) => {
              const pname = v || providerName(a.provider_id);
              return (
                <Space size={6}>
                  <ProviderLogo name={pname} size={18} />
                  <span>{pname}</span>
                </Space>
              );
            },
          },
          { title: t('acct.label'), dataIndex: 'label' },
          { title: t('keys.keyColumn'), dataIndex: 'api_key_mask' },
          { title: t('acct.weight'), dataIndex: 'weight', width: 80 },
          {
            title: t('acct.probes'),
            width: 90,
            render: (_, a) => (a.usage_probes?.length ?? 0) > 0
              ? <Tag>{a.usage_probes.length}</Tag>
              : <Typography.Text type="secondary">-</Typography.Text>,
          },
          {
            title: t('common.enabled'),
            dataIndex: 'enabled',
            width: 90,
            render: (v: boolean, a) => (
              <Switch
                size="small"
                checked={v}
                onChange={async (x) => {
                  await api.put(`/api/v1/accounts/${a.id}`, { enabled: x }).catch((e) => message.error(e.message));
                  load();
                }}
              />
            ),
          },
          {
            title: t('common.actions'),
            render: (_, a) => (
              <Space>
                <AccountTestButton accountId={a.id} />
                <Button size="small" onClick={() => setUsageFor(a)}>{t('acct.usage')}</Button>
                <Button
                  size="small"
                  onClick={() => {
                    setEditing(a);
                    setModalOpen(true);
                  }}
                >
                  {t('common.edit')}
                </Button>
                <Popconfirm
                  title={t('acct.deleteConfirm')}
                  onConfirm={async () => {
                    await api.del(`/api/v1/accounts/${a.id}`).catch((e) => message.error(e.message));
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
      <AccountModal
        open={modalOpen}
        account={editing}
        providers={providers}
        onClose={() => {
          setModalOpen(false);
          setEditing(null);
        }}
        onDone={() => {
          setModalOpen(false);
          setEditing(null);
          load();
        }}
      />
      {usageFor && <AccountUsageModal account={usageFor} onClose={() => setUsageFor(null)} />}
    </div>
  );
}

// — create/edit modal --------------------------------------------------------

interface AccountFormValues {
  provider_id?: number;
  label: string;
  api_key?: string;
  weight: number;
}

function AccountModal({
  open,
  account,
  providers,
  onClose,
  onDone,
}: {
  open: boolean;
  /** Set when editing; null = create. */
  account: Account | null;
  providers: Provider[];
  onClose: () => void;
  onDone: () => void;
}) {
  const { t } = useLang();
  const { message } = AntApp.useApp();
  const [form] = Form.useForm<AccountFormValues>();
  const isEdit = account != null;
  const [probes, setProbes] = useState<UsageProbe[]>([]);

  useEffect(() => {
    if (!open) return;
    form.resetFields();
    if (account) {
      form.setFieldsValue({ label: account.label, weight: account.weight });
      setProbes(account.usage_probes ?? []);
    } else {
      form.setFieldsValue({ weight: 1 });
      setProbes([]);
    }
  }, [open, account, form]); // eslint-disable-line react-hooks/exhaustive-deps

  const submit = async (v: AccountFormValues) => {
    try {
      if (isEdit && account) {
        await api.put(`/api/v1/accounts/${account.id}`, {
          label: v.label ?? '',
          weight: v.weight ?? 1,
          usage_probes: probes,
        });
        message.success(t('prov.saved'));
      } else {
        await api.post(`/api/v1/providers/${v.provider_id}/accounts`, {
          label: v.label ?? '',
          api_key: v.api_key,
          weight: v.weight ?? 1,
          usage_probes: probes,
        });
        message.success(t('acct.added'));
      }
      onDone();
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    }
  };

  return (
    <Modal
      title={isEdit ? t('acct.edit') : t('acct.add')}
      open={open}
      onCancel={onClose}
      onOk={() => form.submit()}
      width={560}
      destroyOnClose
    >
      <Form form={form} layout="vertical" onFinish={submit}>
        {!isEdit && (
          <Form.Item name="provider_id" label={t('acct.provider')} rules={[{ required: true }]}>
            <Select
              options={providers.map((p) => ({ value: p.id, label: p.name }))}
              placeholder={t('acct.provider')}
            />
          </Form.Item>
        )}
        <Form.Item name="label" label={t('acct.label')}>
          <Input placeholder="main" />
        </Form.Item>
        {!isEdit && (
          <Form.Item name="api_key" label={t('acct.apiKey')} rules={[{ required: true }]}>
            <Input.Password placeholder="sk-…" />
          </Form.Item>
        )}
        {isEdit && (
          <Typography.Paragraph type="secondary" style={{ fontSize: 12 }}>
            {t('acct.keyImmutable')}
          </Typography.Paragraph>
        )}
        <Form.Item name="weight" label={t('acct.weight')}>
          <InputNumber min={1} />
        </Form.Item>
        <Typography.Title level={5}>{t('acct.probes')}</Typography.Title>
        <ProbesEditor value={probes} onChange={setProbes} />
      </Form>
    </Modal>
  );
}

// AccountPickerModal pops before actions that authenticate upstream (model
// list fetching): pick a saved account of the provider or paste a key for an
// unsaved one. The choice is always explicit.
export function AccountPickerModal({
  open,
  providerId,
  onPick,
  onClose,
}: {
  open: boolean;
  providerId: number;
  onPick: (cred: { account_id?: number; api_key?: string }) => void;
  onClose: () => void;
}) {
  const { t } = useLang();
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [source, setSource] = useState('');
  const [pasteKey, setPasteKey] = useState('');

  useEffect(() => {
    if (!open) return;
    setPasteKey('');
    api.get<Account[]>(`/api/v1/providers/${providerId}/accounts`)
      .then((list) => {
        const all = list ?? [];
        setAccounts(all);
        const first = all.find((a) => a.enabled);
        setSource(first ? `acc:${first.id}` : 'paste');
      })
      .catch(() => {});
  }, [open, providerId]);

  const valid = source.startsWith('acc:') || (source === 'paste' && pasteKey.trim() !== '');
  const confirm = () => {
    if (source.startsWith('acc:')) onPick({ account_id: Number(source.slice(4)) });
    else onPick({ api_key: pasteKey.trim() });
  };

  return (
    <Modal
      title={t('acct.pickForFetch')}
      open={open}
      onCancel={onClose}
      onOk={confirm}
      okButtonProps={{ disabled: !valid }}
      width={560}
      destroyOnClose
    >
      <Form layout="vertical">
        <Form.Item label={t('acct.provider')}>
          <Select
            value={source}
            onChange={setSource}
            options={[
              ...accounts.map((a) => ({
                value: `acc:${a.id}`,
                label: `${a.label || a.api_key_mask} (${a.api_key_mask})${a.enabled ? '' : ' (off)'}`,
                disabled: !a.enabled,
              })),
              { value: 'paste', label: t('acct.pasteKey') },
            ]}
            notFoundContent={t('acct.noAccounts')}
          />
        </Form.Item>
        {source === 'paste' && (
          <Form.Item label={t('acct.apiKey')} required>
            <Input.Password
              placeholder="sk-…"
              value={pasteKey}
              onChange={(e) => setPasteKey(e.target.value)}
            />
          </Form.Item>
        )}
      </Form>
    </Modal>
  );
}

// — two-tier availability test ------------------------------------------------

export function AccountTestButton({ accountId }: { accountId: number }) {
  const { t } = useLang();
  const { message } = AntApp.useApp();
  const [running, setRunning] = useState(false);
  const [result, setResult] = useState<AccountTestResult | null>(null);

  const run = async (depth: 'quick' | 'deep') => {
    setRunning(true);
    try {
      const r = await api.post<AccountTestResult>(`/api/v1/accounts/${accountId}/test`, { depth });
      setResult(r);
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    } finally {
      setRunning(false);
    }
  };

  return (
    <>
      <Dropdown.Button
        size="small"
        loading={running}
        onClick={() => run('quick')}
        menu={{
          items: [
            { key: 'quick', label: t('acct.test.quick') },
            { key: 'deep', label: t('acct.test.deep') },
          ],
          onClick: ({ key }) => run(key as 'quick' | 'deep'),
        }}
      >
        {t('prov.test')}
      </Dropdown.Button>
      <Modal open={result != null} footer={null} onCancel={() => setResult(null)} title={t('acct.test.title')}>
        {result && (
          <div>
            <div>
              {result.ok ? (
                <Typography.Text type="success">✓ {t('prov.testOk')}</Typography.Text>
              ) : (
                <Typography.Text type="danger">✗ {t(`prov.test.class.${result.class ?? 'unreachable'}`)}</Typography.Text>
              )}
              {' · '}
              <Typography.Text type="secondary">
                {result.depth === 'quick' ? t('acct.test.quick') : t('acct.test.deep')}
                {result.status ? ` · HTTP ${result.status}` : ''}
                {(result.total_ms ?? 0) > 0 ? ` · ${result.total_ms}ms` : ''}
                {result.model_count != null ? ` · ${result.model_count} models` : ''}
                {result.model ? ` · ${result.model}` : ''}
              </Typography.Text>
            </div>
            {result.error && (
              <Typography.Text type="secondary" style={{ fontSize: 12, wordBreak: 'break-all' }}>
                {result.error}
              </Typography.Text>
            )}
            {(result.models?.length ?? 0) > 0 && (
              <div style={{ marginTop: 8 }}>
                {(result.models ?? []).map((m) => (
                  <Tag key={m}>{m}</Tag>
                ))}
              </div>
            )}
          </div>
        )}
      </Modal>
    </>
  );
}

// — usage / balance probes ----------------------------------------------------

function AccountUsageModal({ account, onClose }: { account: Account; onClose: () => void }) {
  const { t } = useLang();
  const { message } = AntApp.useApp();
  const [report, setReport] = useState<AccountUsageReport | null>(null);
  const [loading, setLoading] = useState(false);

  // Without refresh=1 the 60s TTL cache serves repeats; force bypasses it.
  const query = useCallback(async (force: boolean) => {
    setLoading(true);
    try {
      const r = await api.post<AccountUsageReport>(`/api/v1/accounts/${account.id}/usage${force ? '?refresh=1' : ''}`);
      setReport(r);
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    } finally {
      setLoading(false);
    }
  }, [account.id, message]);

  // Open = query immediately: results show without a second click.
  useEffect(() => { query(false); }, [query]);

  return (
    <Modal
      title={`${t('acct.usage')} — ${account.label || account.api_key_mask}`}
      open
      onCancel={onClose}
      footer={null}
      width={640}
    >
      <Space style={{ marginBottom: 12 }} align="center">
        <Button type="primary" size="small" icon={<SyncOutlined spin={loading} />} onClick={() => query(true)} disabled={loading}>
          {loading ? t('prov.testing') : t('common.refresh')}
        </Button>
        {report && (
          <Typography.Text type="secondary">
            {t('prov.usageQueriedAt')} {new Date(report.queried_at * 1000).toLocaleTimeString()}
          </Typography.Text>
        )}
      </Space>
      {report && (report.results?.length ?? 0) === 0 && (
        <Typography.Text type="secondary">
          {report.configured ? t('prov.usageNoKeys') : t('prov.usageNotConfigured')}
        </Typography.Text>
      )}
      {report && !report.configured && (report.results?.length ?? 0) > 0 && (
        <div style={{ marginBottom: 4 }}>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {t('prov.usageDefaults')}
          </Typography.Text>
        </div>
      )}
      {(report?.results ?? []).map((r, i) => (
        <ProbeResultView key={i} r={r} />
      ))}
    </Modal>
  );
}

/** ProbesEditor is the controlled probe list (add/edit/delete via ProbeModal). */
export function ProbesEditor({
  value,
  onChange,
}: {
  value: UsageProbe[];
  onChange: (probes: UsageProbe[]) => void;
}) {
  const { t } = useLang();
  const [modalOpen, setModalOpen] = useState(false);
  const [editIdx, setEditIdx] = useState<number | null>(null);

  const submitProbe = (p: UsageProbe) => {
    onChange(editIdx != null ? value.map((x, j) => (j === editIdx ? p : x)) : [...value, p]);
    setModalOpen(false);
    setEditIdx(null);
  };

  return (
    <>
      <Button
        size="small"
        icon={<PlusOutlined />}
        style={{ marginBottom: 12 }}
        onClick={() => {
          setEditIdx(null);
          setModalOpen(true);
        }}
      >
        {t('prov.usage.addProbe')}
      </Button>
      {value.map((p, i) => (
        <Space key={i} style={{ display: 'flex', marginBottom: 8 }} align="center" wrap>
          <Tag color={p.type === 'plan' ? 'purple' : 'blue'}>
            {p.type === 'plan' ? t('prov.usage.type.plan') : t('prov.usage.type.balance')}
          </Tag>
          <Typography.Text>{p.name || '-'}</Typography.Text>
          <Typography.Text type="secondary" code style={{ fontSize: 12 }}>
            {p.path}
          </Typography.Text>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {p.auth_style ?? 'bearer'}
          </Typography.Text>
          <Button
            size="small"
            onClick={() => {
              setEditIdx(i);
              setModalOpen(true);
            }}
          >
            {t('common.edit')}
          </Button>
          <Popconfirm title={t('common.delete') + '?'} onConfirm={() => onChange(value.filter((_, j) => j !== i))}>
            <Button danger size="small">{t('common.delete')}</Button>
          </Popconfirm>
        </Space>
      ))}
      {value.length === 0 && (
        <div style={{ marginBottom: 8 }}>
          <Typography.Text type="secondary">{t('prov.usageNotConfigured')}</Typography.Text>
        </div>
      )}
      <ProbeModal
        open={modalOpen}
        initial={editIdx != null ? value[editIdx] : null}
        onSubmit={submitProbe}
        onClose={() => {
          setModalOpen(false);
          setEditIdx(null);
        }}
      />
    </>
  );
}

export function ProbeModal({
  open,
  initial,
  onSubmit,
  onClose,
}: {
  open: boolean;
  initial: UsageProbe | null;
  onSubmit: (p: UsageProbe) => void;
  onClose: () => void;
}) {
  const { t } = useLang();
  const [form] = Form.useForm<UsageProbe>();

  useEffect(() => {
    if (!open) return;
    form.resetFields();
    form.setFieldsValue(initial ?? { type: 'balance', path: '', name: '', auth_style: 'bearer' });
  }, [open, initial, form]);

  return (
    <Modal
      title={initial ? `${t('common.edit')} — ${initial.name || initial.path}` : t('prov.usage.addProbe')}
      open={open}
      onCancel={onClose}
      onOk={() => form.submit()}
      width={560}
      destroyOnClose
    >
      <Form form={form} layout="vertical" onFinish={onSubmit}>
        <Space size="large">
          <Form.Item name="type" label={t('prov.usage')} rules={[{ required: true }]}>
            <Select
              style={{ width: 140 }}
              options={[
                { value: 'balance', label: t('prov.usage.type.balance') },
                { value: 'plan', label: t('prov.usage.type.plan') },
              ]}
            />
          </Form.Item>
          <Form.Item name="name" label={t('prov.usage.probeName')}>
            <Input placeholder="Balance" style={{ width: 180 }} />
          </Form.Item>
          <Form.Item name="auth_style" label={t('prov.usage.probeAuth')}>
            <Select
              style={{ width: 120 }}
              options={[
                { value: 'bearer', label: 'Bearer' },
                { value: 'raw', label: 'Raw' },
              ]}
            />
          </Form.Item>
        </Space>
        <Form.Item name="path" label="URL" rules={[{ required: true }]}>
          <Input placeholder="https://api.deepseek.com/user/balance" />
        </Form.Item>
      </Form>
    </Modal>
  );
}

export function ProbeResultView({ r }: { r: ProbeResult }) {
  const { t } = useLang();
  if (!r.ok) {
    return (
      <div style={{ marginTop: 8 }}>
        <Typography.Text type="warning">
          ⚠ {r.probe}: {r.error}
          {r.status ? ` (HTTP ${r.status})` : ''}
        </Typography.Text>
        {r.raw && (
          <Typography.Text type="secondary" style={{ fontSize: 12, wordBreak: 'break-all', display: 'block' }}>
            {r.raw.slice(0, 200)}
          </Typography.Text>
        )}
      </div>
    );
  }
  return (
    <div style={{ marginTop: 8 }}>
      <Typography.Text type="secondary">
        {r.probe}
        {r.plan ? ` · ${r.plan}` : ''}
        {r.path ? (
          <Typography.Text type="secondary" code style={{ fontSize: 11, marginLeft: 8 }}>
            {r.path}
          </Typography.Text>
        ) : null}
      </Typography.Text>
      {r.balance && <BalanceView b={r.balance} />}
      {(r.windows ?? []).map((w, i) => (
        <WindowBar key={i} w={w} />
      ))}
      {r.raw && (
        <Collapse
          size="small"
          style={{ marginTop: 6 }}
          items={[
            {
              key: 'raw',
              label: <span style={{ fontSize: 12 }}>{t('prov.usage.raw')}</span>,
              children: (
                <pre style={{ margin: 0, fontSize: 11, whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>
                  {r.raw}
                </pre>
              ),
            },
          ]}
        />
      )}
    </div>
  );
}

// QuotaRow is the shared row layout for every usage/balance bar: label left,
// secondary metrics right, progress bar below — one style for both balance
// and plan windows. Bars are the normal accent color; `dangerAt` opts a bar
// into turning red past a threshold (usage windows near exhaustion). Balance
// bars never turn red: a high paid share is healthy, not a warning.
function QuotaRow({
  label,
  meta,
  percent,
  dangerAt,
}: {
  label: ReactNode;
  meta: ReactNode;
  percent?: number;
  dangerAt?: number;
}) {
  const p = percent != null ? Math.min(100, Math.max(0, percent)) : undefined;
  const status = p != null && dangerAt != null && p >= dangerAt ? 'exception' : 'normal';
  return (
    <div style={{ marginTop: 6, maxWidth: 460 }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', gap: 8 }}>
        <Typography.Text style={{ fontSize: 12 }}>{label}</Typography.Text>
        <Typography.Text type="secondary" style={{ fontSize: 12, textAlign: 'right' }}>
          {meta}
        </Typography.Text>
      </div>
      {p != null && <Progress percent={p} size="small" status={status} style={{ marginBottom: 0 }} />}
    </div>
  );
}

function BalanceView({ b }: { b: UsageBalance }) {
  const { t } = useLang();
  const cur = b.currency || '¥';
  const money = (v?: number) => (v == null ? null : `${cur}${v.toFixed(2)}`);
  const bits: string[] = [];
  if (b.total != null) bits.push(money(b.total)!);
  if (b.available != null && b.available !== b.total) bits.push(`${money(b.available)}`);
  if (b.granted != null && b.granted > 0) bits.push(`(${money(b.granted)})`);
  if (b.voucher != null) bits.push(`[${money(b.voucher)}]`);

  // Progress bar: paid portion of the total when derivable (DeepSeek-style
  // total minus granted, or Kimi cash share of available); else fill by
  // available/total.
  let pct: number | undefined;
  if (b.total != null && b.granted != null && b.total > 0) {
    pct = ((b.total - b.granted) / b.total) * 100;
  } else if (b.available != null && b.cash != null && b.available > 0) {
    pct = (b.cash / b.available) * 100;
  } else if (b.available != null && b.total != null && b.total > 0) {
    pct = (b.available / b.total) * 100;
  }
  return (
    <QuotaRow
      label={t('prov.usage.type.balance')}
      meta={bits.join(' · ')}
      percent={pct}
    />
  );
}

function WindowBar({ w }: { w: UsageWindow }) {
  const { t } = useLang();
  const winText =
    w.window === '5h'
      ? t('prov.usage.win.5h')
      : w.window === '7d'
        ? t('prov.usage.win.7d')
        : w.window === '30d'
          ? t('prov.usage.win.30d')
          : (w.window ?? '');
  const label = [w.name, winText].filter(Boolean).join(' · ');
  const fmtAmt = (v?: number) =>
    v == null ? null : Number.isInteger(v) ? String(v) : v.toFixed(1);
  const amounts =
    w.used_amount != null && w.limit_amount != null
      ? ` (${fmtAmt(w.used_amount)}/${fmtAmt(w.limit_amount)}${w.unit ? ` ${w.unit}` : ''})`
      : '';
  const meta = (
    <>
      {w.unlimited
        ? t('prov.usage.unlimited')
        : w.used_percent != null
          ? `${t('prov.usage.used')} ${w.used_percent.toFixed(1)}%${amounts}`
          : w.remaining_percent != null
            ? `${t('prov.usage.remaining')} ${w.remaining_percent.toFixed(1)}%`
            : (w.note ?? '')}
      {w.resets_at ? ` · ${t('prov.usage.resetAt')} ${new Date(w.resets_at * 1000).toLocaleString()}` : ''}
    </>
  );
  return <QuotaRow label={label} meta={meta} percent={w.used_percent ?? undefined} dangerAt={90} />;
}
