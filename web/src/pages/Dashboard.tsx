import { useEffect, useMemo, useState } from 'react';
import { Card, Col, Empty, Row, Segmented, Select, Space, Statistic, Table, Spin } from 'antd';
import { Line } from '@ant-design/plots';
import { api } from '../api/client';
import { useLang } from '../i18n/i18n';
import { useTheme } from '../theme/theme';

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
  cache_read_tokens: number;
}

interface ChartDatum {
  bucket: string;
  model: string;
  requests: number;
  prompt_tokens: number;
  completion_tokens: number;
  cache_hit_rate: number;
}

// 1234 → 1.2K, 5.6M, 7.8G for the token charts' axis, tooltips and stat cards.
const compactNumber = (v: number): string => {
  const abs = Math.abs(v);
  if (abs >= 1e9) return `${+(v / 1e9).toFixed(1)}G`;
  if (abs >= 1e6) return `${+(v / 1e6).toFixed(1)}M`;
  if (abs >= 1e3) return `${+(v / 1e3).toFixed(1)}K`;
  return `${v}`;
};

// 1234567 → 1,234,567 for table cells.
const commaNumber = (v: number): string => Number(v).toLocaleString('en-US');

// 850 → "850ms", 12345 → "12.3s" for the latency stat card.
const formatMs = (v: number): string => (v >= 1000 ? `${+(v / 1000).toFixed(1)}s` : `${v}ms`);

export default function Dashboard() {
  const { t } = useLang();
  const { mode } = useTheme();
  const [ov, setOv] = useState<Overview | null>(null);
  const [rows, setRows] = useState<GroupRow[]>([]);
  // Empty selection means "all groups"; only groups with traffic are options.
  const [selectedModels, setSelectedModels] = useState<string[]>([]);
  const [groupBy, setGroupBy] = useState<'model' | 'account'>('model');
  const [view, setView] = useState<'chart' | 'table'>('chart');

  useEffect(() => {
    api.get<Overview>('/api/v1/stats/overview').then(setOv).catch(() => {});
  }, []);
  useEffect(() => {
    setSelectedModels([]);
    api
      .get<GroupRow[]>(`/api/v1/stats/timeseries?group_by=${groupBy}`)
      .then((x) => setRows(x ?? []))
      .catch(() => {});
  }, [groupBy]);

  // Reshape bucket×model rows into a zero-filled chart series: every model gets
  // a point on every observed day so lines drop to 0 instead of breaking.
  // The axis always covers the last 7 days (UTC, matching the backend bucket
  // granularity) even when some days have no traffic. Models with all-zero
  // totals (never actually used) are dropped entirely.
  const { chartData, models } = useMemo(() => {
    const days: string[] = [];
    const today = new Date();
    for (let i = 6; i >= 0; i--) {
      days.push(
        new Date(Date.UTC(today.getUTCFullYear(), today.getUTCMonth(), today.getUTCDate() - i))
          .toISOString()
          .slice(0, 10),
      );
    }
    const active = [...new Set(rows.map((r) => r.group))]
      .sort()
      .filter((m) =>
        rows.some(
          (r) =>
            r.group === m &&
            r.requests + r.prompt_tokens + r.completion_tokens + r.cache_read_tokens > 0,
        ),
      );
    const byKey = new Map(rows.map((r) => [`${r.bucket}|${r.group}`, r]));
    const out: ChartDatum[] = [];
    for (const bucket of days) {
      for (const model of active) {
        const r = byKey.get(`${bucket}|${model}`);
        const prompt = r?.prompt_tokens ?? 0;
        const cacheRead = r?.cache_read_tokens ?? 0;
        const hitRate =
          prompt + cacheRead > 0
            ? Math.round((cacheRead / (prompt + cacheRead)) * 1000) / 10
            : 0;
        out.push({
          bucket,
          model,
          requests: r?.requests ?? 0,
          prompt_tokens: prompt,
          completion_tokens: r?.completion_tokens ?? 0,
          cache_hit_rate: hitRate,
        });
      }
    }
    return { chartData: out, models: active };
  }, [rows]);

  // Empty selection means "all models".
  const filteredData = useMemo(
    () =>
      selectedModels.length === 0
        ? chartData
        : chartData.filter((d) => selectedModels.includes(d.model)),
    [chartData, selectedModels],
  );
  const filteredRows = useMemo(
    () =>
      selectedModels.length === 0
        ? rows
        : rows.filter((r) => selectedModels.includes(r.group)),
    [rows, selectedModels],
  );

  if (!ov) return <Spin />;

  // Tooltip rows sorted by value, largest first.
  const tooltipSort = (d: { value?: number }) => -Number(d.value ?? 0);

  const charts: {
    title: string;
    yField: keyof ChartDatum & string;
    percent?: boolean;
    compact?: boolean;
  }[] = [
    { title: t('dash.promptTokens'), yField: 'prompt_tokens', compact: true },
    { title: t('dash.completionTokens'), yField: 'completion_tokens', compact: true },
    { title: t('dash.requests'), yField: 'requests' },
    { title: t('dash.cacheHitRate'), yField: 'cache_hit_rate', percent: true },
  ];

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
              value={compactNumber(ov.prompt_tokens)}
              suffix={`/ ${compactNumber(ov.completion_tokens)}`}
            />
          </Card>
        </Col>
        <Col span={6}>
          <Card>
            <Statistic
              title={t('dash.latency')}
              value={ov.latency_p50_ms == null ? '-' : formatMs(ov.latency_p50_ms)}
              suffix={`/ ${ov.latency_p95_ms == null ? '-' : formatMs(ov.latency_p95_ms)}`}
            />
          </Card>
        </Col>
      </Row>
      {ov.dropped_logs > 0 && (
        <Card style={{ marginTop: 16 }}>
          {t('dash.dropped')}: {ov.dropped_logs}
        </Card>
      )}
      <Card
        title={groupBy === 'model' ? t('dash.byModel') : t('dash.byAccount')}
        style={{ marginTop: 16 }}
        extra={
          <Space wrap>
            <Segmented
              value={groupBy}
              onChange={(v) => setGroupBy(v as 'model' | 'account')}
              options={[
                { label: t('dash.model'), value: 'model' },
                { label: t('dash.account'), value: 'account' },
              ]}
            />
            <Select
              mode="multiple"
              allowClear
              showSearch
              placeholder={t('dash.modelFilter')}
              style={{ minWidth: 260 }}
              maxTagCount="responsive"
              options={models.map((m) => ({ value: m, label: m }))}
              value={selectedModels}
              onChange={setSelectedModels}
            />
            <Segmented
              value={view}
              onChange={(v) => setView(v as 'chart' | 'table')}
              options={[
                { label: t('dash.viewChart'), value: 'chart' },
                { label: t('dash.viewTable'), value: 'table' },
              ]}
            />
          </Space>
        }
      >
        {view === 'table' ? (
          <Table<GroupRow>
            size="small"
            rowKey={(r) => r.bucket + r.group}
            dataSource={filteredRows.slice(0, 20)}
            columns={[
              { title: t('dash.day'), dataIndex: 'bucket' },
              { title: groupBy === 'model' ? t('dash.model') : t('dash.account'), dataIndex: 'group' },
              { title: t('dash.requests'), dataIndex: 'requests', render: commaNumber },
              { title: t('dash.promptTokens'), dataIndex: 'prompt_tokens', render: commaNumber },
              { title: t('dash.completionTokens'), dataIndex: 'completion_tokens', render: commaNumber },
            ]}
            pagination={false}
          />
        ) : filteredData.length === 0 ? (
          <Empty />
        ) : (
          <Row gutter={[16, 16]}>
            {charts.map((c) => (
              <Col xs={24} lg={12} key={c.yField}>
                <Card size="small" title={c.title}>
                  <Line
                    data={filteredData}
                    xField="bucket"
                    yField={c.yField}
                    colorField="model"
                    theme={mode === 'dark' ? 'classicDark' : 'classic'}
                    height={260}
                    smooth
                    point={{ size: 3 }}
                    tooltip={{
                      sort: tooltipSort,
                      items: [
                        {
                          channel: 'y',
                          valueFormatter: (v: number) =>
                            c.percent ? `${v}%` : c.compact ? compactNumber(v) : `${v}`,
                        },
                      ],
                    }}
                    {...(c.compact
                      ? {
                          axis: {
                            y: { labelFormatter: (v: number) => compactNumber(Number(v)) },
                          },
                        }
                      : {})}
                    {...(c.percent
                      ? { scale: { y: { domainMin: 0, domainMax: 100 } } }
                      : {})}
                  />
                </Card>
              </Col>
            ))}
          </Row>
        )}
      </Card>
    </div>
  );
}
