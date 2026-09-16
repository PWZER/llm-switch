import { App as AntApp, Button, Layout, Menu, Segmented } from 'antd';
import {
  DashboardOutlined,
  CloudServerOutlined,
  AppstoreOutlined,
  SwapOutlined,
  KeyOutlined,
  FileTextOutlined,
  SettingOutlined,
  LogoutOutlined,
} from '@ant-design/icons';
import { Outlet, useLocation, useNavigate } from 'react-router-dom';
import { api, clearToken } from '../api/client';
import { useLang } from '../i18n/i18n';

const { Sider, Header, Content } = Layout;

export default function AdminLayout() {
  const nav = useNavigate();
  const loc = useLocation();
  const { message } = AntApp.useApp();
  const { t, lang, setLang } = useLang();

  const items = [
    { key: '/', icon: <DashboardOutlined />, label: t('nav.dashboard') },
    { key: '/providers', icon: <CloudServerOutlined />, label: t('nav.providers') },
    { key: '/models', icon: <AppstoreOutlined />, label: t('nav.models') },
    { key: '/aliases', icon: <SwapOutlined />, label: t('nav.aliases') },
    { key: '/client-keys', icon: <KeyOutlined />, label: t('nav.clientKeys') },
    { key: '/logs', icon: <FileTextOutlined />, label: t('nav.logs') },
    { key: '/settings', icon: <SettingOutlined />, label: t('nav.settings') },
  ];

  const logout = async () => {
    try {
      await api.post('/api/v1/auth/logout');
    } catch {
      // token already invalid: fine
    }
    clearToken();
    message.success(t('common.logout'));
    nav('/login');
  };

  return (
    <Layout style={{ minHeight: '100vh' }}>
      <Sider theme="dark" width={200}>
        <div style={{ color: '#fff', padding: 16, fontWeight: 700, fontSize: 16 }}>
          {t('app.title')}
        </div>
        <Menu
          theme="dark"
          mode="inline"
          selectedKeys={[loc.pathname]}
          items={items}
          onClick={({ key }) => nav(key)}
        />
      </Sider>
      <Layout>
        <Header
          style={{
            background: '#fff',
            display: 'flex',
            justifyContent: 'flex-end',
            alignItems: 'center',
            gap: 16,
            paddingInline: 24,
          }}
        >
          <Segmented
            value={lang}
            onChange={(v) => setLang(v as 'en' | 'zh')}
            options={[
              { value: 'en', label: 'EN' },
              { value: 'zh', label: '中文' },
            ]}
          />
          <Button icon={<LogoutOutlined />} onClick={logout}>
            {t('common.logout')}
          </Button>
        </Header>
        <Content style={{ margin: 16 }}>
          <p style={{ marginTop: 0, marginBottom: 12, color: '#888' }}>{t('app.subtitle')}</p>
          <Outlet />
        </Content>
      </Layout>
    </Layout>
  );
}
