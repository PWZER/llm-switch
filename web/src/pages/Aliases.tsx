import { useCallback, useEffect, useState } from 'react';
import {
  App as AntApp, Button, Form, Input, Modal, Popconfirm, Select, Space, Table, Typography,
} from 'antd';
import { PlusOutlined, SwapOutlined } from '@ant-design/icons';
import { api, Alias, Channel } from '../api/client';
import { useLang } from '../i18n/i18n';

// Aliases are the hot-switch layer: agents keep model "main" (or any frozen
// name) forever while the admin re-points the target here — zero restarts.
export default function Aliases() {
  const { t } = useLang();
  const { message } = AntApp.useApp();
  const [rows, setRows] = useState<Alias[]>([]);
  const [channels, setChannels] = useState<Channel[]>([]);
  const [open, setOpen] = useState(false);
  const [editing, setEditing] = useState<Alias | null>(null);
  const [form] = Form.useForm<{ name: string; channel_id: number; upstream_model: string }>();

  const load = useCallback(() => {
    api.get<Alias[]>('/api/v1/aliases').then((x) => setRows(x ?? [])).catch((e) => message.error(e.message));
    api.get<Channel[]>('/api/v1/channels').then((x) => setChannels(x ?? [])).catch(() => {});
  }, [message]);
  useEffect(load, [load]);

  const save = async (v: { name: string; channel_id: number; upstream_model: string }) => {
    try {
      await api.put(`/api/v1/aliases/${v.name}`, v);
      message.success(`"${v.name}" ${t('alias.switched')}`);
      setOpen(false);
      load();
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'failed');
    }
  };

  return (
    <div>
      <Typography.Paragraph type="secondary">{t('alias.tip')}</Typography.Paragraph>
      <Space style={{ marginBottom: 16 }}>
        <Button
          type="primary"
          icon={<PlusOutlined />}
          onClick={() => {
            setEditing(null);
            form.resetFields();
            setOpen(true);
          }}
        >
          {t('alias.new')}
        </Button>
        <Button onClick={load}>{t('common.reload')}</Button>
      </Space>
      <Table<Alias>
        rowKey="name"
        dataSource={rows}
        columns={[
          { title: t('alias.name'), dataIndex: 'name', render: (n: string) => <code>{n}</code> },
          {
            title: t('alias.targetChannel'),
            render: (_, a) => {
              const c = channels.find((x) => x.id === a.channel_id);
              return c ? `${c.name} (${c.protocol}, ${c.provider_name})` : `#${a.channel_id}`;
            },
          },
          { title: t('alias.upstreamModel'), dataIndex: 'upstream_model' },
          {
            title: t('common.actions'),
            render: (_, a) => (
              <Space>
                <Button
                  size="small"
                  icon={<SwapOutlined />}
                  onClick={() => {
                    setEditing(a);
                    form.setFieldsValue(a);
                    setOpen(true);
                  }}
                >
                  {t('alias.switchTarget')}
                </Button>
                <Popconfirm
                  title={t('alias.deleteConfirm')}
                  onConfirm={async () => {
                    await api.del(`/api/v1/aliases/${a.name}`);
                    load();
                  }}
                >
                  <Button danger size="small">{t('common.delete')}</Button>
                </Popconfirm>
              </Space>
            ),
          },
        ]}
      />
      <Modal
        title={editing ? `${t('alias.switchTarget')} — ${editing.name}` : t('alias.new')}
        open={open}
        onCancel={() => setOpen(false)}
        onOk={() => form.submit()}
      >
        <Form form={form} layout="vertical" onFinish={save}>
          <Form.Item name="name" label={t('alias.name')} rules={[{ required: true }]}>
            <Input placeholder={t('alias.namePlaceholder')} disabled={!!editing} />
          </Form.Item>
          <Form.Item name="channel_id" label={t('alias.targetChannel')} rules={[{ required: true }]}>
            <Select
              options={channels
                .filter((c) => c.enabled)
                .map((c) => ({
                  value: c.id,
                  label: `${c.name} (${c.protocol}, ${c.provider_name})`,
                }))}
            />
          </Form.Item>
          <Form.Item name="upstream_model" label={t('alias.upstreamModel')} rules={[{ required: true }]}>
            <Input placeholder="deepseek-chat / glm-5.3 / ..." />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  );
}
