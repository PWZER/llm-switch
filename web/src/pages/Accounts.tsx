import { useCallback, useEffect, useState } from 'react';
import { App as AntApp } from 'antd';
import { api, Provider } from '../api/client';
import { AccountsTable } from '../components/account';

// Account Pool: every upstream account across all providers in one table.
// Accounts always belong to a provider — the create modal forces the pick.
export default function Accounts() {
  const { message } = AntApp.useApp();
  const [providers, setProviders] = useState<Provider[]>([]);

  const load = useCallback(() => {
    api.get<Provider[]>('/api/v1/providers')
      .then((x) => setProviders(x ?? []))
      .catch((e) => message.error(e.message));
  }, [message]);
  useEffect(load, [load]);

  return <AccountsTable providers={providers} onRefresh={load} />;
}
