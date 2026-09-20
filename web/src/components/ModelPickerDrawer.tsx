import { useEffect, useMemo, useState } from 'react';
import { App as AntApp, Button, Drawer, Popconfirm, Select, Space, Typography } from 'antd';
import { SyncOutlined } from '@ant-design/icons';
import { api, Account, Model, Provider } from '../api/client';
import { useLang } from '../i18n/i18n';
import ModelSelectList, { ModelSelectItem } from './ModelSelectList';

// ModelPickerDrawer is the Models page's "fetch from provider" flow: pick a
// provider + authenticating account, fetch the upstream list (refresh-models
// only lists — nothing registers), then apply the selection as an explicit
// add/remove diff through sync-models. The list is the union of the fetch
// result and the provider's current rows: registered ids start checked
// (unchecking removes them on confirm), and registered ids the upstream no
// longer lists are tagged for easy cleanup.
export default function ModelPickerDrawer({
  open,
  onClose,
  onRegistered,
}: {
  open: boolean;
  onClose: () => void;
  onRegistered: () => void;
}) {
  const { t } = useLang();
  const { message } = AntApp.useApp();

  const [providers, setProviders] = useState<Provider[]>([]);
  const [providerId, setProviderId] = useState<number | undefined>();
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [accountId, setAccountId] = useState<number | undefined>();

  const [items, setItems] = useState<ModelSelectItem[]>([]);
  const [value, setValue] = useState<string[]>([]);
  const [registeredIds, setRegisteredIds] = useState<Set<string>>(new Set());
  const [fetching, setFetching] = useState(false);
  const [syncing, setSyncing] = useState(false);

  useEffect(() => {
    if (!open) return;
    setProviders([]);
    setProviderId(undefined);
    setAccounts([]);
    setAccountId(undefined);
    setItems([]);
    setValue([]);
    setRegisteredIds(new Set());
    api.get<Provider[]>('/api/v1/providers').then((x) => setProviders(x ?? [])).catch(() => {});
  }, [open]);

  useEffect(() => {
    if (providerId == null) {
      setAccounts([]);
      setAccountId(undefined);
      return;
    }
    api.get<Account[]>(`/api/v1/providers/${providerId}/accounts`)
      .then((list) => {
        const enabled = (list ?? []).filter((a) => a.enabled);
        setAccounts(list ?? []);
        setAccountId(enabled[0]?.id);
      })
      .catch(() => {});
  }, [providerId]);

  const fetchList = async () => {
    if (providerId == null || accountId == null) return;
    setFetching(true);
    try {
      const [r, rows] = await Promise.all([
        api.post<{ models: { id: string; context_length: number | null; max_output_tokens: number | null }[] }>(
          `/api/v1/providers/${providerId}/refresh-models`,
          { account_id: accountId },
        ),
        api.get<Model[]>('/api/v1/models'),
      ]);
      const fetched = r.models ?? [];
      const mine = (rows ?? []).filter((m) => m.provider_id === providerId);
      const reg = new Set(mine.map((m) => m.id));
      const fetchedLimits = new Map(fetched.map((m) => [m.id, m]));
      // Union of the fetched list and the provider's current rows: rows the
      // upstream no longer lists stay visible (stale) so they can be removed.
      const merged: ModelSelectItem[] = fetched.map((m) => ({
        id: m.id,
        context_window: m.context_length,
        max_output_tokens: m.max_output_tokens,
        registered: reg.has(m.id),
        stale: false,
      }));
      for (const row of mine) {
        if (fetchedLimits.has(row.id)) continue;
        merged.push({
          id: row.id,
          context_window: row.context_window,
          max_output_tokens: row.max_output_tokens,
          registered: true,
          stale: true,
        });
      }
      setItems(merged);
      setRegisteredIds(reg);
      setValue(Array.from(reg)); // registered ids start checked; unchecking removes
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    } finally {
      setFetching(false);
    }
  };

  const addCount = useMemo(() => value.filter((id) => !registeredIds.has(id)).length, [value, registeredIds]);
  const removeCount = useMemo(
    () => Array.from(registeredIds).filter((id) => !value.includes(id)).length,
    [value, registeredIds],
  );

  const applySync = async () => {
    if (providerId == null) return;
    setSyncing(true);
    try {
      const limitsById = new Map(items.map((m) => [m.id, m]));
      const register = value
        .filter((id) => !registeredIds.has(id))
        .map((id) => ({
          id,
          context_length: limitsById.get(id)?.context_window ?? null,
          max_output_tokens: limitsById.get(id)?.max_output_tokens ?? null,
        }));
      const remove = Array.from(registeredIds).filter((id) => !value.includes(id));
      const r = await api.post<{ models_added: number; models_removed: number }>(
        `/api/v1/providers/${providerId}/sync-models`,
        { register, remove },
      );
      message.success(
        `${t('models.synced')} (+${r.models_added} / -${r.models_removed})`,
      );
      onRegistered();
      onClose();
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    } finally {
      setSyncing(false);
    }
  };

  const applyButton = (
    <Button
      type="primary"
      loading={syncing}
      disabled={addCount === 0 && removeCount === 0}
      onClick={removeCount > 0 ? undefined : applySync}
    >
      {t('models.syncBtn')} (+{addCount} / -{removeCount})
    </Button>
  );

  return (
    <Drawer
      title={t('models.fetch')}
      open={open}
      onClose={onClose}
      width={960}
    >
      {open && (
        <>
          <Typography.Title level={5}>{t('models.pickSource')}</Typography.Title>
          <Space wrap>
            <Select
              value={providerId}
              onChange={setProviderId}
              options={providers.map((p) => ({ value: p.id, label: p.name }))}
              placeholder={t('acct.provider')}
              style={{ width: 280 }}
            />
            <Select
              value={accountId}
              onChange={setAccountId}
              options={accounts.map((a) => ({
                value: a.id,
                label: `${a.label || a.api_key_mask} (${a.api_key_mask})${a.enabled ? '' : ' (off)'}`,
                disabled: !a.enabled,
              }))}
              placeholder={t('models.fetchAccount')}
              style={{ width: 280 }}
              notFoundContent={t('acct.noAccounts')}
            />
            <Button
              icon={<SyncOutlined />}
              loading={fetching}
              disabled={providerId == null || accountId == null}
              onClick={fetchList}
            >
              {t('models.fetchList')}
            </Button>
          </Space>
          {items.length > 0 && (
            <>
              <Typography.Title level={5} style={{ marginTop: 24 }}>
                {t('models.pickModels')}
              </Typography.Title>
              <Typography.Paragraph type="secondary" style={{ fontSize: 12 }}>
                {t('models.pickHint')}
              </Typography.Paragraph>
              <ModelSelectList items={items} value={value} onChange={setValue} />
              <div style={{ marginTop: 16 }}>
                {removeCount > 0 ? (
                  <Popconfirm title={t('models.removeConfirm')} onConfirm={applySync}>
                    {applyButton}
                  </Popconfirm>
                ) : (
                  applyButton
                )}
              </div>
            </>
          )}
        </>
      )}
    </Drawer>
  );
}
