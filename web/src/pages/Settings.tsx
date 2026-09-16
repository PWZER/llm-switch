import { useCallback, useEffect, useState } from 'react';
import {
  App as AntApp, Button, Card, Form, Input, InputNumber, Space, Typography,
} from 'antd';
import { api } from '../api/client';
import { useLang } from '../i18n/i18n';

interface SettingsMap {
  retention_days?: string;
  max_failover_attempts?: string;
  default_max_tokens?: string;
  stream_idle_timeout_s?: string;
  auto_bind_new_models?: string;
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
        {t('settings.tip1')} <code>ANTHROPIC_BASE_URL=http://your-host:8080</code> {t('settings.tip2')}{' '}
        <code>x-api-key</code>; {t('settings.tip3')}{' '}
        <code>base_url=http://your-host:8080/v1</code>.
      </Typography.Paragraph>
    </Space>
  );
}
