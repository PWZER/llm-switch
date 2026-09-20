import { useCallback, useEffect, useState } from 'react';
import {
  Alert, App as AntApp, Button, Card, Checkbox, Form, Input, InputNumber, Space,
  Typography, Upload,
} from 'antd';
import type { UploadFile } from 'antd';
import { UploadOutlined } from '@ant-design/icons';
import {
  api, ConfigExport, ConfigSection, CONFIG_EXPORT_SECTIONS, ImportCounts, ImportResult,
} from '../api/client';
import { useLang } from '../i18n/i18n';

interface SettingsMap {
  retention_days?: string;
  max_failover_attempts?: string;
  default_max_tokens?: string;
  stream_idle_timeout_s?: string;
  log_bodies?: string;
  [k: string]: string | undefined;
}

export default function Settings() {
  const { t } = useLang();
  const { message } = AntApp.useApp();
  const [form] = Form.useForm();
  const [saving, setSaving] = useState(false);

  const load = useCallback(() => {
    api.get<SettingsMap>('/api/v1/settings').then((s) =>
      form.setFieldsValue({
        retention_days: Number(s.retention_days ?? 30),
        max_failover_attempts: Number(s.max_failover_attempts ?? 3),
        default_max_tokens: Number(s.default_max_tokens ?? 8192),
        stream_idle_timeout_s: Number(s.stream_idle_timeout_s ?? 300),
      }),
    );
  }, [form]);
  useEffect(load, [load]);

  const save = async (v: Record<string, number>) => {
    setSaving(true);
    try {
      const body = Object.fromEntries(Object.entries(v).map(([k, x]) => [k, String(x)]));
      await api.put('/api/v1/settings', body);
      message.success(t('settings.saved'));
      load();
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    } finally {
      setSaving(false);
    }
  };

  const changePassword = async (v: { old: string; new: string }) => {
    try {
      await api.put('/api/v1/auth/password', v);
      message.success(t('settings.passwordChanged'));
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    }
  };

  return (
    <Space direction="vertical" size="large" style={{ width: '100%' }}>
      <Card title={t('settings.gateway')} style={{ maxWidth: 560 }}>
        <Form form={form} layout="vertical" onFinish={save}>
          <Form.Item name="retention_days" label={t('settings.retention')} rules={[{ required: true }]}>
            <InputNumber min={1} style={{ width: '100%' }} />
          </Form.Item>
          <Form.Item
            name="max_failover_attempts"
            label={t('settings.failover')}
            tooltip={t('settings.failoverTip')}
            rules={[{ required: true }]}
          >
            <InputNumber min={1} style={{ width: '100%' }} />
          </Form.Item>
          <Form.Item
            name="default_max_tokens"
            label={t('settings.defaultMaxTokens')}
            tooltip={t('settings.defaultMaxTokensTip')}
            rules={[{ required: true }]}
          >
            <InputNumber min={1} style={{ width: '100%' }} />
          </Form.Item>
          <Form.Item
            name="stream_idle_timeout_s"
            label={t('settings.idleTimeout')}
            tooltip={t('settings.idleTimeoutTip')}
            rules={[{ required: true }]}
          >
            <InputNumber min={10} style={{ width: '100%' }} />
          </Form.Item>
          <Button type="primary" htmlType="submit" loading={saving}>
            {t('common.save')}
          </Button>
        </Form>
      </Card>

      <ExportCard />

      <ImportCard />

      <Card title={t('settings.password')} style={{ maxWidth: 560 }}>
        <Form layout="vertical" onFinish={changePassword}>
          <Form.Item name="old" label={t('settings.currentPassword')} rules={[{ required: true }]}>
            <Input.Password />
          </Form.Item>
          <Form.Item
            name="new"
            label={t('settings.newPassword')}
            rules={[{ required: true, min: 8, message: t('settings.newPasswordRule') }]}
          >
            <Input.Password />
          </Form.Item>
          <Button htmlType="submit">{t('common.save')}</Button>
        </Form>
      </Card>

      <Typography.Paragraph type="secondary" style={{ maxWidth: 560 }}>
        {t('settings.tip1')} <code>ANTHROPIC_BASE_URL=http://your-host:8901</code> {t('settings.tip2')}{' '}
        <code>x-api-key</code>; {t('settings.tip3')}{' '}
        <code>base_url=http://your-host:8901/v1</code>.
      </Typography.Paragraph>
    </Space>
  );
}

// ExportCard downloads the selected config sections as a JSON document.
// Account API keys are plaintext upstream credentials, so including them is
// opt-out but loudly warned about.
function ExportCard() {
  const { t } = useLang();
  const { message } = AntApp.useApp();
  const [sections, setSections] = useState<ConfigSection[]>([...CONFIG_EXPORT_SECTIONS]);
  const [includeKeys, setIncludeKeys] = useState(true);
  const [exporting, setExporting] = useState(false);

  const exportConfig = async () => {
    setExporting(true);
    try {
      const doc = await api.post<ConfigExport>('/api/v1/config/export', {
        sections,
        include_account_keys: includeKeys,
      });
      const pad = (n: number) => String(n).padStart(2, '0');
      const d = new Date();
      const stamp = `${d.getFullYear()}${pad(d.getMonth() + 1)}${pad(d.getDate())}-${pad(d.getHours())}${pad(d.getMinutes())}${pad(d.getSeconds())}`;
      const blob = new Blob([JSON.stringify(doc, null, 2)], { type: 'application/json' });
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = `llm-switch-config-${stamp}.json`;
      a.click();
      URL.revokeObjectURL(url);
      message.success(t('settings.exportDone'));
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    } finally {
      setExporting(false);
    }
  };

  return (
    <Card title={t('settings.export')} style={{ maxWidth: 560 }}>
      <Space direction="vertical" style={{ width: '100%' }}>
        <Typography.Text strong>{t('settings.exportSections')}</Typography.Text>
        <Checkbox.Group
          options={CONFIG_EXPORT_SECTIONS.map((s) => ({
            label: t(`settings.exportSection.${s}`),
            value: s,
          }))}
          value={sections}
          onChange={(v) => setSections(v as ConfigSection[])}
        />
        <Checkbox checked={includeKeys} onChange={(e) => setIncludeKeys(e.target.checked)}>
          {t('settings.exportIncludeKeys')}
        </Checkbox>
        {includeKeys && <Alert type="warning" showIcon message={t('settings.exportWarning')} />}
        <Button type="primary" loading={exporting} disabled={sections.length === 0} onClick={exportConfig}>
          {t('settings.exportBtn')}
        </Button>
      </Space>
    </Card>
  );
}

// Per-section activity summary used for both the dry-run preview and the
// final import result: only sections that touch any row are listed.
function PlanCounts({ result }: { result: ImportResult }) {
  const { t } = useLang();
  const rows: [string, ImportCounts][] = (
    [
      [t('settings.exportSection.providers'), result.providers],
      [t('settings.importSection.accounts'), result.accounts],
      [t('settings.importSection.channels'), result.channels],
      [t('settings.importSection.models'), result.models],
      [t('settings.exportSection.model_routes'), result.model_routes],
      [t('settings.exportSection.api_keys'), result.api_keys],
      [t('settings.exportSection.settings'), result.settings],
    ] as [string, ImportCounts][]
  ).filter(([, n]) => n.created > 0 || n.updated > 0 || n.skipped > 0);
  return (
    <ul style={{ margin: '8px 0 0', paddingLeft: 18 }}>
      {rows.map(([label, n]) => (
        <li key={label}>
          {label}: {n.created} {t('settings.importCreated')}, {n.updated} {t('settings.importUpdated')}
          {n.skipped > 0 && `, ${n.skipped} ${t('settings.importSkipped')}`}
        </li>
      ))}
    </ul>
  );
}

function WarningsList({ warnings }: { warnings: string[] }) {
  return (
    <ul style={{ margin: 0, paddingLeft: 18 }}>
      {warnings.map((w, i) => (
        <li key={i}>{w}</li>
      ))}
    </ul>
  );
}

// ImportCard reads a config document client-side, previews it, runs a dry
// run whose counts + warnings gate the confirmation, then applies.
function ImportCard() {
  const { t } = useLang();
  const { message, modal } = AntApp.useApp();
  const [doc, setDoc] = useState<ConfigExport | null>(null);
  const [fileList, setFileList] = useState<UploadFile[]>([]);
  const [planning, setPlanning] = useState(false);
  const [result, setResult] = useState<ImportResult | null>(null);

  const clearFile = () => {
    setDoc(null);
    setFileList([]);
  };

  const parseFile = (file: File) => {
    file.text()
      .then((txt) => {
        const parsed = JSON.parse(txt) as ConfigExport;
        const hasContent = !!parsed && parsed.version === 1 && (
          (Array.isArray(parsed.providers) && parsed.providers.length > 0) ||
          (Array.isArray(parsed.model_routes) && parsed.model_routes.length > 0) ||
          (Array.isArray(parsed.api_keys) && parsed.api_keys.length > 0) ||
          (!!parsed.settings && Object.keys(parsed.settings).length > 0)
        );
        if (!hasContent) throw new Error('bad document');
        setDoc(parsed);
        setResult(null);
        setFileList([{ uid: 'import-file', name: file.name, status: 'done' }]);
      })
      .catch(() => {
        message.error(t('settings.importBadFile'));
        clearFile();
      });
  };

  const runImport = async () => {
    if (!doc) return;
    try {
      const res = await api.post<ImportResult>('/api/v1/config/import', doc);
      setResult(res);
      message.success(t('settings.importDone'));
      clearFile();
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    }
  };

  const planAndConfirm = async () => {
    if (!doc) return;
    setPlanning(true);
    try {
      const plan = await api.post<ImportResult>('/api/v1/config/import?dry_run=1', doc);
      modal.confirm({
        title: t('settings.importConfirmTitle'),
        content: (
          <div>
            <Typography.Paragraph type="secondary" style={{ marginBottom: 0 }}>
              {t('settings.importDesc')}
            </Typography.Paragraph>
            <PlanCounts result={plan} />
            {(plan.warnings ?? []).length > 0 && (
              <Alert
                type="warning"
                showIcon
                style={{ marginTop: 8 }}
                message={t('settings.importWarnings')}
                description={<WarningsList warnings={plan.warnings} />}
              />
            )}
          </div>
        ),
        okText: t('settings.importBtn'),
        okType: 'danger',
        onOk: runImport,
      });
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    } finally {
      setPlanning(false);
    }
  };

  return (
    <Card title={t('settings.import')} style={{ maxWidth: 560 }}>
      <Space direction="vertical" style={{ width: '100%' }}>
        <Typography.Paragraph type="secondary" style={{ marginBottom: 0 }}>
          {t('settings.importDesc')}
        </Typography.Paragraph>
        <Upload
          accept=".json,application/json"
          maxCount={1}
          fileList={fileList}
          beforeUpload={(file) => {
            parseFile(file);
            return false;
          }}
          onRemove={clearFile}
        >
          <Button icon={<UploadOutlined />}>{t('settings.importChoose')}</Button>
        </Upload>
        {doc && (
          <>
            <Typography.Text strong>{t('settings.importPreview')}</Typography.Text>
            <ImportSummary doc={doc} />
            <Button type="primary" danger loading={planning} onClick={planAndConfirm}>
              {t('settings.importBtn')}
            </Button>
          </>
        )}
        {result && (
          <Alert
            type={(result.warnings ?? []).length > 0 ? 'warning' : 'success'}
            showIcon
            message={t('settings.importDone')}
            description={
              <>
                <PlanCounts result={result} />
                {(result.warnings ?? []).length > 0 && (
                  <>
                    <Typography.Text strong style={{ display: 'block', marginTop: 8 }}>
                      {t('settings.importWarnings')}
                    </Typography.Text>
                    <WarningsList warnings={result.warnings} />
                  </>
                )}
              </>
            }
          />
        )}
      </Space>
    </Card>
  );
}

// ImportSummary shows what the uploaded document contains, computed
// client-side from the parsed file.
function ImportSummary({ doc }: { doc: ConfigExport }) {
  const { t } = useLang();
  const prov = doc.providers ?? [];
  const accounts = prov.reduce((n, p) => n + (p.accounts?.length ?? 0), 0);
  const channels = prov.reduce((n, p) => n + (p.channels?.length ?? 0), 0);
  const models = prov.reduce((n, p) => n + (p.models?.length ?? 0), 0);
  const lines = [
    `${t('settings.exportSection.providers')}: ${prov.length} (${accounts} / ${channels} / ${models})`,
    `${t('settings.exportSection.model_routes')}: ${(doc.model_routes ?? []).length}`,
    `${t('settings.exportSection.api_keys')}: ${(doc.api_keys ?? []).length}`,
    `${t('settings.exportSection.settings')}: ${Object.keys(doc.settings ?? {}).length}`,
  ];
  return (
    <Typography.Paragraph type="secondary" style={{ marginBottom: 0, whiteSpace: 'pre-line' }}>
      {lines.join('\n')}
    </Typography.Paragraph>
  );
}
