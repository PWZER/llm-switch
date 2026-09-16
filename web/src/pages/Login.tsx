import { useState } from 'react';
import { Button, Card, Form, Input, Typography, theme } from 'antd';
import { useNavigate } from 'react-router-dom';
import { api, setToken } from '../api/client';
import { useLang } from '../i18n/i18n';

export default function Login() {
  const nav = useNavigate();
  const { t } = useLang();
  const { token } = theme.useToken();
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');

  const onFinish = async ({ password }: { password: string }) => {
    setLoading(true);
    setError('');
    try {
      const data = await api.post<{ token: string }>('/api/v1/auth/login', { password });
      setToken(data.token);
      nav('/');
    } catch (e) {
      setError(e instanceof Error ? e.message : t('login.failed'));
    } finally {
      setLoading(false);
    }
  };

  return (
    <div
      style={{
        minHeight: '100vh',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        background: token.colorBgLayout,
      }}
    >
      <Card style={{ width: 360 }}>
        <Typography.Title level={4} style={{ textAlign: 'center' }}>
          {t('app.title')}
        </Typography.Title>
        <Form onFinish={onFinish} layout="vertical">
          <Form.Item name="password" label={t('login.title')} rules={[{ required: true }]}>
            <Input.Password autoFocus />
          </Form.Item>
          {error && <Typography.Text type="danger">{error}</Typography.Text>}
          <Button type="primary" htmlType="submit" block loading={loading}>
            {t('login.signIn')}
          </Button>
        </Form>
      </Card>
    </div>
  );
}
