import { useCallback, useEffect, useState } from 'react';
import {
  App as AntApp, Button, Form, Input, Modal, Popconfirm, Space, Switch, Table, Typography,
} from 'antd';
import { PlusOutlined } from '@ant-design/icons';
import { api, ClientKey } from '../api/client';
import { useLang } from '../i18n/i18n';

export default function ClientKeys() {
  const { t } = useLang();
  const { message } = AntApp.useApp();
  const [rows, setRows] = useState<ClientKey[]>([]);
  const [open, setOpen] = useState(false);
  const [created, setCreated] = useState('');
  const [form] = Form.useForm<{ name: string }>();

  const load = useCallback(() => {
    api.get<ClientKey[]>('/api/v1/client-keys').then((x) => setRows(x ?? [])).catch((e) => message.error(e.message));
  }, [message]);
  useEffect(load, [load]);

  const create = async (v: { name: string }) => {
    try {
      const data = await api.post<{ key: string }>('/api/v1/client-keys', v);
      setCreated(data.key);
      setOpen(false);
      form.resetFields();
      load();
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    }
  };

  return (
    <div>
      <Space style={{ marginBottom: 16 }}>
        <Button type="primary" icon={<PlusOutlined />} onClick={() => setOpen(true)}>
          {t('keys.new')}
        </Button>
        <Button onClick={load}>{t('common.refresh')}</Button>
      </Space>
      <Table<ClientKey>
        rowKey="id"
        dataSource={rows}
        columns={[
          { title: t('common.name'), dataIndex: 'name' },
          { title: t('keys.keyColumn'), dataIndex: 'prefix' },
          {
            title: t('common.enabled'),
            dataIndex: 'enabled',
            render: (v: boolean, k) => (
              <Switch
                size="small"
                checked={v}
                onChange={async (x) => {
                  await api.put(`/api/v1/client-keys/${k.id}`, { enabled: x });
                  load();
                }}
              />
            ),
          },
          {
            title: '',
            render: (_, k) => (
              <Popconfirm
                title={t('keys.revokeConfirm')}
                onConfirm={async () => {
                  await api.del(`/api/v1/client-keys/${k.id}`);
                  load();
                }}
              >
                <Button danger size="small">{t('keys.revoke')}</Button>
              </Popconfirm>
            ),
          },
        ]}
      />
      <Modal title={t('keys.new')} open={open} onCancel={() => setOpen(false)} onOk={() => form.submit()}>
        <Form form={form} layout="vertical" onFinish={create}>
          <Form.Item name="name" label={t('keys.name')} rules={[{ required: true }]}>
            <Input placeholder={t('keys.namePlaceholder')} />
          </Form.Item>
        </Form>
      </Modal>
      <Modal
        title={t('keys.createdTitle')}
        open={!!created}
        onCancel={() => setCreated('')}
        footer={[
          <Button key="ok" type="primary" onClick={() => setCreated('')}>
            {t('keys.savedIt')}
          </Button>,
        ]}
      >
        <Typography.Paragraph>{t('keys.createdOnce')}</Typography.Paragraph>
        <Typography.Paragraph copyable code style={{ wordBreak: 'break-all' }}>
          {created}
        </Typography.Paragraph>
      </Modal>
    </div>
  );
}
