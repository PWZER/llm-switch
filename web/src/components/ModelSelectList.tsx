import { useMemo, useState } from 'react';
import { Input, Table, Tag, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { useLang } from '../i18n/i18n';

/** One selectable entry of a fetched (or merged) upstream model list. */
export interface ModelSelectItem {
  id: string;
  context_window?: number | null;
  max_output_tokens?: number | null;
  /** The id already has a registry row on the target provider. */
  registered: boolean;
  /** Registered but absent from the freshly fetched list (upstream dropped it). */
  stale: boolean;
}

// Token counts render in compact K/M units (1024-base, the convention for
// context windows: 131072 -> 128K, 1048576 -> 1M). Display only.
export const formatTokens = (v: number | null): string => {
  if (v == null) return '-';
  const trim = (n: number): string =>
    Number.isInteger(n) ? String(n) : n.toFixed(2).replace(/0+$/, '').replace(/\.$/, '');
  if (v >= 1024 * 1024) return `${trim(v / (1024 * 1024))}M`;
  if (v >= 1024) return `${trim(v / 1024)}K`;
  return String(v);
};

// ModelSelectList is the shared searchable table behind the model picker
// flows: `value` is the controlled checked-id set. With `disableRegistered`
// (provider-drawer staging) registered rows render checked and locked — they
// are already saved and cannot be re-staged; otherwise (sync drawer) they
// toggle freely so unchecking means removal.
export default function ModelSelectList({
  items,
  value,
  onChange,
  disableRegistered = false,
}: {
  items: ModelSelectItem[];
  value: string[];
  onChange: (next: string[]) => void;
  disableRegistered?: boolean;
}) {
  const { t } = useLang();
  const [search, setSearch] = useState('');

  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase();
    return items
      .filter((m) => !q || m.id.toLowerCase().includes(q))
      .sort((a, b) => a.id.localeCompare(b.id));
  }, [items, search]);

  const columns: ColumnsType<ModelSelectItem> = [
    {
      title: t('models.modelId'),
      dataIndex: 'id',
      render: (_, m) => (
        <>
          <span style={{ fontFamily: 'monospace', fontSize: 13 }}>{m.id}</span>
          {m.registered && (
            <Tag color="green" style={{ marginLeft: 8 }}>
              {t('models.alreadyAdded')}
            </Tag>
          )}
          {m.stale && (
            <Tag color="red" style={{ marginLeft: 4 }}>
              {t('models.staleTag')}
            </Tag>
          )}
        </>
      ),
    },
    { title: t('models.context'), dataIndex: 'context_window', width: 120, render: formatTokens },
    { title: t('models.maxOutput'), dataIndex: 'max_output_tokens', width: 120, render: formatTokens },
  ];

  return (
    <div>
      <Input
        placeholder={t('models.searchModels')}
        style={{ width: 220, marginBottom: 12 }}
        value={search}
        onChange={(e) => setSearch(e.target.value)}
        allowClear
      />
      <Table<ModelSelectItem>
        rowKey={(m) => m.id}
        dataSource={filtered}
        columns={columns}
        pagination={false}
        rowSelection={{
          selectedRowKeys: value,
          onChange: (keys) => onChange(keys.map(String)),
          getCheckboxProps: (m) => ({ disabled: disableRegistered && m.registered }),
        }}
      />
      {filtered.length === 0 && (
        <Typography.Text type="secondary">{t('models.noMatches')}</Typography.Text>
      )}
    </div>
  );
}
