import { useCallback, useEffect, useState } from 'react';
import { App as AntApp, Button, DatePicker, Input, Select, Space, Table, Tag, Tooltip } from 'antd';
import dayjs from 'dayjs';
import { api, RequestLog } from '../api/client';
import { useLang } from '../i18n/i18n';
import { ProtocolTag } from '../components/protocol';

export default function Logs() {
  const { t } = useLang();
  const { message } = AntApp.useApp();
  const [items, setItems] = useState<RequestLog[]>([]);
  const [total, setTotal] = useState(0);
  const [page, setPage] = useState(1);
  const [pageSize] = useState(50);
  const [model, setModel] = useState('');
  const [status, setStatus] = useState<number | undefined>();
  const [range, setRange] = useState<[number, number] | null>(null);

  const load = useCallback(() => {
    const params = new URLSearchParams({ page: String(page), page_size: String(pageSize) });
    if (model) params.set('model', model);
    if (status) params.set('status', String(status));
    if (range) {
      params.set('from', String(range[0]));
      params.set('to', String(range[1]));
    }
    api.get<{ items: RequestLog[]; total: number }>(`/api/v1/logs?${params}`)
      .then((d) => {
        setItems(d.items ?? []);
        setTotal(d.total);
      })
      .catch((e) => message.error(e.message));
  }, [page, pageSize, model, status, range, message]);
  useEffect(load, [load]);

  const columns = [
    {
      title: t('logs.time'),
      dataIndex: 'ts',
      width: 110,
      render: (ts: number) => dayjs(ts).format('MM-DD HH:mm:ss'),
    },
    { title: t('logs.model'), dataIndex: 'model' },
    { title: t('logs.upstream'), dataIndex: 'upstream_model' },
    {
      title: t('logs.path'),
      render: (_: unknown, l: RequestLog) =>
        l.protocol_in === l.protocol_out ? (
          <ProtocolTag protocol={l.protocol_in} />
        ) : (
          <Space size={4} wrap>
            <ProtocolTag protocol={l.protocol_in} />
            <span style={{ opacity: 0.6 }}>→</span>
            <ProtocolTag protocol={l.protocol_out} />
          </Space>
        ),
    },
    { title: t('logs.key'), dataIndex: 'api_key_name' },
    { title: t('logs.channel'), dataIndex: 'channel_name' },
    {
      title: t('logs.account'),
      dataIndex: 'account_name',
      render: (v: string) => v || <span style={{ opacity: 0.4 }}>-</span>,
    },
    {
      title: t('logs.status'),
      dataIndex: 'status',
      width: 80,
      render: (s: number, l: RequestLog) =>
        l.success ? <Tag color="green">{s}</Tag> : <Tag color="red">{s}</Tag>,
    },
    {
      title: t('logs.tokens'),
      render: (_: unknown, l: RequestLog) =>
        `${l.prompt_tokens} / ${l.completion_tokens} / ${l.cache_read_tokens}`,
    },
    {
      title: t('logs.latency'),
      render: (_: unknown, l: RequestLog) =>
        `${l.latency_ms}ms / ${l.first_token_ms == null ? '-' : l.first_token_ms + 'ms'}`,
    },
    { title: t('logs.tries'), dataIndex: 'attempts', width: 60 },
    {
      title: '',
      dataIndex: 'error_type',
      render: (e: string | null) =>
        e ? (
          <Tooltip title={e}>
            <Tag color="orange">{e}</Tag>
          </Tooltip>
        ) : null,
    },
  ];

  return (
    <div>
      <div style={{ marginBottom: 16, display: 'flex', gap: 8, flexWrap: 'wrap' }}>
        <Input
          placeholder={t('dash.model')}
          style={{ width: 200 }}
          value={model}
          onChange={(e) => {
            setPage(1);
            setModel(e.target.value);
          }}
          allowClear
        />
        <Select
          placeholder={t('logs.status')}
          style={{ width: 120 }}
          allowClear
          onChange={(v) => {
            setPage(1);
            setStatus(v);
          }}
          options={[200, 400, 401, 404, 429, 500, 502].map((s) => ({ value: s, label: s }))}
        />
        <DatePicker.RangePicker
          showTime
          onChange={(v) => {
            setPage(1);
            setRange(
              v?.[0] && v?.[1] ? [v[0].valueOf(), v[1].valueOf()] : null,
            );
          }}
        />
        <Button onClick={load}>{t('common.search')}</Button>
      </div>
      <Table<RequestLog>
        rowKey="id"
        dataSource={items}
        columns={columns}
        pagination={{
          current: page,
          pageSize,
          total,
          onChange: (p) => setPage(p),
          showTotal: (n) => `${n} ${t('logs.totalRequests')}`,
        }}
      />
    </div>
  );
}
