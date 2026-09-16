import { useEffect, useState } from 'react';
import { Card, Col, Row, Statistic, Table, Spin } from 'antd';
import { api } from '../api/client';
import { useLang } from '../i18n/i18n';

interface Overview {
  requests: number;
  successes: number;
  success_rate: number;
  prompt_tokens: number;
  completion_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  reasoning_tokens: number;
  latency_p50_ms: number | null;
  latency_p95_ms: number | null;
  dropped_logs: number;
}

interface GroupRow {
  bucket: string;
  group: string;
  requests: number;
  prompt_tokens: number;
  completion_tokens: number;
}

export default function Dashboard() {
  const { t } = useLang();
  const [ov, setOv] = useState<Overview | null>(null);
  const [rows, setRows] = useState<GroupRow[]>([]);

  useEffect(() => {
    api.get<Overview>('/api/v1/stats/overview').then(setOv).catch(() => {});
    api
      .get<GroupRow[]>('/api/v1/stats/timeseries?group_by=model')
      .then((x) => setRows(x ?? []))
      .catch(() => {});
  }, []);

  if (!ov) return <Spin />;

  return (
    <div>
      <Row gutter={16}>
        <Col span={6}>
          <Card>
            <Statistic title={t('dash.requests24h')} value={ov.requests} />
          </Card>
        </Col>
        <Col span={6}>
          <Card>
            <Statistic
              title={t('dash.successRate')}
              value={(ov.success_rate * 100).toFixed(1)}
              suffix="%"
            />
          </Card>
        </Col>
        <Col span={6}>
          <Card>
            <Statistic
              title={t('dash.tokens')}
              value={ov.prompt_tokens}
              suffix={`/ ${ov.completion_tokens}`}
            />
          </Card>
        </Col>
        <Col span={6}>
          <Card>
            <Statistic
              title={t('dash.latency')}
              value={ov.latency_p50_ms ?? '-'}
              suffix={`/ ${ov.latency_p95_ms ?? '-'} ms`}
            />
          </Card>
        </Col>
      </Row>
      {ov.dropped_logs > 0 && (
        <Card style={{ marginTop: 16 }}>
          {t('dash.dropped')}: {ov.dropped_logs}
        </Card>
      )}
      <Card title={t('dash.byModel')} style={{ marginTop: 16 }}>
        <Table<GroupRow>
          size="small"
          rowKey={(r) => r.bucket + r.group}
          dataSource={rows.slice(0, 20)}
          columns={[
            { title: t('dash.day'), dataIndex: 'bucket' },
            { title: t('dash.model'), dataIndex: 'group' },
            { title: t('dash.requests'), dataIndex: 'requests' },
            { title: t('dash.promptTokens'), dataIndex: 'prompt_tokens' },
            { title: t('dash.completionTokens'), dataIndex: 'completion_tokens' },
          ]}
          pagination={false}
        />
      </Card>
    </div>
  );
}
