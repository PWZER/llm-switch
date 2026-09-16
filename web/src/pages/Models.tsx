import { useCallback, useEffect, useState } from 'react';
import {
  App as AntApp, Button, Form, Input, InputNumber, Modal, Popconfirm, Space, Switch, Table, Tag,
} from 'antd';
import { PlusOutlined, SyncOutlined } from '@ant-design/icons';
import { api, Model, Provider } from '../api/client';
import { useLang } from '../i18n/i18n';

export default function Models() {
  const { t } = useLang();
  const { message } = AntApp.useApp();
  const [rows, setRows] = useState<Model[]>([]);
  const [providers, setProviders] = useState<Provider[]>([]);
  const [refreshing, setRefreshing] = useState(false);
  const [open, setOpen] = useState(false);
  const [form] = Form.useForm();

  const load = useCallback(() => {
    api.get<Model[]>('/api/v1/models').then((x) => setRows(x ?? [])).catch((e) => message.error(e.message));
    api.get<Provider[]>('/api/v1/providers').then((x) => setProviders(x ?? [])).catch(() => {});
  }, [message]);
  useEffect(load, [load]);

  const create = async (v: { id: string; display_name?: string; context_window?: number }) => {
    try {
      await api.post('/api/v1/models', v);
      message.success(t('common.ok'));
      setOpen(false);
      form.resetFields();
      load();
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    }
  };

  const toggle = async (m: Model, enabled: boolean) => {
    await api.put(`/api/v1/models/${m.id}`, { enabled }).catch(() => message.error('failed'));
    load();
  };

  // Fetch each provider's upstream model list (needs a configured endpoint+key).
  const refreshUpstream = async () => {
    setRefreshing(true);
    let total = 0;
    for (const p of providers) {
      try {
        const r = await api.post<{ models_added: number }>(
          `/api/v1/providers/${p.id}/refresh-models`,
        );
        total += r.models_added;
      } catch {
        // per-provider failure is non-fatal
      }
    }
    setRefreshing(false);
    message.success(`${t('models.fetched')} (${total} ${t('models.new')})`);
    load();
  };

  return (
    <div>
      <Space style={{ marginBottom: 16 }}>
        <Button type="primary" icon={<PlusOutlined />} onClick={() => setOpen(true)}>
          {t('models.add')}
        </Button>
        <Button icon={<SyncOutlined />} loading={refreshing} onClick={refreshUpstream}>
          {t('models.fetch')}
        </Button>
        <Button onClick={load}>{t('common.reload')}</Button>
      </Space>
      <Table<Model>
        rowKey="id"
        dataSource={rows}
        columns={[
          { title: t('dash.model'), dataIndex: 'id' },
          { title: t('models.displayName'), dataIndex: 'display_name' },
          {
            title: t('models.source'),
            dataIndex: 'source',
            render: (s: string) => <Tag>{s}</Tag>,
          },
          {
            title: t('models.context'),
            dataIndex: 'context_window',
            render: (v: number | null) => v ?? '-',
          },
          {
            title: t('common.enabled'),
            dataIndex: 'enabled',
            render: (v: boolean, m) => <Switch size="small" checked={v} onChange={(x) => toggle(m, x)} />,
          },
          {
            title: '',
            render: (_, m) => (
              <Popconfirm
                title={t('models.deleteConfirm')}
                onConfirm={async () => {
                  await api.del(`/api/v1/models/${m.id}`);
                  load();
                }}
              >
                <Button danger size="small">{t('common.delete')}</Button>
              </Popconfirm>
            ),
          },
        ]}
      />
      <Modal
        title={t('models.add')}
        open={open}
        onCancel={() => setOpen(false)}
        onOk={() => form.submit()}
      >
        <Form form={form} layout="vertical" onFinish={create}>
          <Form.Item name="id" label={t('models.modelId')} rules={[{ required: true }]}>
            <Input placeholder={t('models.displayPlaceholder')} />
          </Form.Item>
          <Form.Item name="display_name" label={t('models.displayName')}>
            <Input />
          </Form.Item>
          <Form.Item name="context_window" label={t('models.context')}>
            <InputNumber style={{ width: '100%' }} />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  );
}
