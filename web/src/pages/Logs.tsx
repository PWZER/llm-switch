import { useCallback, useEffect, useState } from 'react';
import {
  App as AntApp, Button, Collapse, DatePicker, Descriptions, Drawer, Empty, Input,
  Select, Space, Spin, Table, Tabs, Tag, Tooltip, Typography,
} from 'antd';
import { CopyOutlined } from '@ant-design/icons';
import dayjs from 'dayjs';
import { api, LogPayload, LogPayloadSegment, RequestLog } from '../api/client';
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
  const [payloadFor, setPayloadFor] = useState<RequestLog | null>(null);

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
      width: 150,
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
    {
      title: t('logs.account'),
      dataIndex: 'account_name',
      render: (v: string) => v || <span style={{ opacity: 0.4 }}>-</span>,
    },
    {
      title: t('logs.status'),
      dataIndex: 'status',
      width: 140,
      render: (s: number, l: RequestLog) => (
        <Space size={4}>
          {l.success ? <Tag color="green">{s}</Tag> : <Tag color="red">{s}</Tag>}
          {l.payload_path !== '' && (
            <Tooltip title={t('logs.payloadTip')}>
              <Tag color="blue">{t('logs.payload')}</Tag>
            </Tooltip>
          )}
        </Space>
      ),
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
        onRow={(l) => ({
          onClick: () => {
            if (l.payload_path !== '') setPayloadFor(l);
          },
          style: l.payload_path !== '' ? { cursor: 'pointer' } : {},
        })}
        pagination={{
          current: page,
          pageSize,
          total,
          onChange: (p) => setPage(p),
          showTotal: (n) => `${n} ${t('logs.totalRequests')}`,
        }}
      />
      <LogPayloadDrawer log={payloadFor} onClose={() => setPayloadFor(null)} />
    </div>
  );
}

// LogPayloadDrawer shows the three recorded segments (client request,
// upstream request, upstream response) of one log row.
function LogPayloadDrawer({ log, onClose }: { log: RequestLog | null; onClose: () => void }) {
  const { t } = useLang();
  const [payload, setPayload] = useState<LogPayload | null>(null);
  const [loading, setLoading] = useState(false);
  const [missing, setMissing] = useState(false);

  useEffect(() => {
    if (!log) return;
    setPayload(null);
    setMissing(false);
    setLoading(true);
    api.get<LogPayload>(`/api/v1/logs/${log.id}/payload`)
      .then(setPayload)
      .catch(() => setMissing(true))
      .finally(() => setLoading(false));
  }, [log]);

  return (
    <Drawer
      title={
        log && (
          <Space size={8}>
            <span>{t('logs.detail')}</span>
            {log.success ? <Tag color="green">{log.status}</Tag> : <Tag color="red">{log.status}</Tag>}
          </Space>
        )
      }
      width={720}
      open={log != null}
      onClose={onClose}
      destroyOnHidden
    >
      {log && (
        <Descriptions
          size="small"
          column={2}
          style={{ marginBottom: 16 }}
          items={[
            { key: 'time', label: t('logs.time'), children: dayjs(log.ts).format('YYYY-MM-DD HH:mm:ss') },
            { key: 'model', label: t('logs.model'), children: log.model },
            {
              key: 'path',
              label: t('logs.path'),
              children: (
                <Space size={4}>
                  <ProtocolTag protocol={log.protocol_in} />
                  {log.protocol_in !== log.protocol_out && (
                    <>
                      <span style={{ opacity: 0.6 }}>→</span>
                      <ProtocolTag protocol={log.protocol_out} />
                    </>
                  )}
                </Space>
              ),
            },
            { key: 'latency', label: t('logs.latency'), children: `${log.latency_ms}ms` },
          ]}
        />
      )}
      {loading && <Spin style={{ display: 'block', margin: '48px auto' }} />}
      {missing && !loading && <Empty description={t('logs.payloadMissing')} />}
      {payload && !loading && (
        <Tabs
          items={[
            {
              key: 'client',
              label: t('logs.clientRequest'),
              children: <SegmentView segment={payload.client_request} />,
            },
            {
              key: 'upstream',
              label: t('logs.upstreamRequest'),
              children: payload.upstream_request
                ? <SegmentView segment={payload.upstream_request} />
                : <Empty description={t('logs.noUpstream')} />,
            },
            {
              key: 'response',
              label: t('logs.upstreamResponse'),
              children: payload.upstream_response
                ? (
                  <SegmentView
                    segment={payload.upstream_response}
                    status={payload.upstream_response.status}
                  />
                )
                : <Empty description={t('logs.noUpstream')} />,
            },
          ]}
        />
      )}
    </Drawer>
  );
}

// SegmentView renders one recorded HTTP message: collapsible headers plus a
// scrollable body with copy support and JSON pretty-printing.
function SegmentView({ segment, status }: { segment: LogPayloadSegment; status?: number }) {
  const { t } = useLang();
  const { message } = AntApp.useApp();

  const headerLines = Object.entries(segment.headers ?? {})
    .map(([k, vs]) => `${k}: ${vs.join(', ')}`)
    .join('\n');

  let body = segment.body;
  try {
    body = JSON.stringify(JSON.parse(segment.body), null, 2);
  } catch {
    // not JSON (e.g. an SSE stream): show raw
  }

  const copy = () => {
    navigator.clipboard.writeText(segment.body).then(
      () => message.success(t('logs.copied')),
      () => message.error(t('logs.copyFailed')),
    );
  };

  return (
    <div>
      {status != null && (
        <Typography.Paragraph style={{ marginBottom: 8 }}>
          <Tag color={status < 400 ? 'green' : 'red'}>status {status}</Tag>
          {segment.truncated && <Tag color="orange">{t('logs.truncated')}</Tag>}
        </Typography.Paragraph>
      )}
      <Collapse
        size="small"
        style={{ marginBottom: 8 }}
        items={[
          {
            key: 'headers',
            label: t('logs.headers'),
            children: <pre style={preStyle}>{headerLines || '-'}</pre>,
          },
        ]}
      />
      <div style={{ position: 'relative' }}>
        <Button
          size="small"
          icon={<CopyOutlined />}
          style={{ position: 'absolute', top: 4, right: 4, zIndex: 1 }}
          onClick={copy}
        >
          {t('logs.copy')}
        </Button>
        <pre style={{ ...preStyle, maxHeight: 320 }}>{body || '(empty)'}</pre>
      </div>
    </div>
  );
}

const preStyle: React.CSSProperties = {
  margin: 0,
  padding: 8,
  background: 'rgba(0,0,0,0.03)',
  borderRadius: 6,
  fontSize: 12,
  overflow: 'auto',
  whiteSpace: 'pre-wrap',
  wordBreak: 'break-all',
};
