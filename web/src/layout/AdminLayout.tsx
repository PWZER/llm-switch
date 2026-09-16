import { useEffect, useState } from 'react';
import { App as AntApp, Button, Layout, Menu, Space, Tooltip, theme as antdTheme } from 'antd';
import {
  DashboardOutlined,
  CloudServerOutlined,
  TeamOutlined,
  AppstoreOutlined,
  SwapOutlined,
  KeyOutlined,
  FileTextOutlined,
  SettingOutlined,
  LogoutOutlined,
  SunOutlined,
  MoonOutlined,
  TranslationOutlined,
} from '@ant-design/icons';
import { Outlet, useLocation, useNavigate } from 'react-router-dom';
import { api, clearToken } from '../api/client';
import { useLang } from '../i18n/i18n';
import { useTheme } from '../theme/theme';

const { Sider, Header, Content } = Layout;

const SIDER_WIDTH = 200;
const SIDER_COLLAPSED_WIDTH = 64;

export default function AdminLayout() {
  const nav = useNavigate();
  const loc = useLocation();
  const { message } = AntApp.useApp();
  const { t, lang, setLang } = useLang();
  const { mode, toggle } = useTheme();
  const { token } = antdTheme.useToken();

  const [collapsed, setCollapsed] = useState(() => localStorage.getItem('lsw_sider') === '1');
  useEffect(() => {
    localStorage.setItem('lsw_sider', collapsed ? '1' : '0');
  }, [collapsed]);

  const items = [
    { key: '/', icon: <DashboardOutlined />, label: t('nav.dashboard') },
    { key: '/providers', icon: <CloudServerOutlined />, label: t('nav.providers') },
    { key: '/accounts', icon: <TeamOutlined />, label: t('nav.accounts') },
    { key: '/models', icon: <AppstoreOutlined />, label: t('nav.models') },
    { key: '/model-routes', icon: <SwapOutlined />, label: t('nav.modelRoutes') },
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
    <Layout style={{ minHeight: '100vh', background: 'transparent' }}>
      {/* Fixed, viewport-height sider flush to the left edge. */}
      <Sider
        theme="light"
        width={SIDER_WIDTH}
        collapsedWidth={SIDER_COLLAPSED_WIDTH}
        collapsible
        collapsed={collapsed}
        onCollapse={setCollapsed}
        style={{
          position: 'fixed',
          top: 0,
          left: 0,
          bottom: 0,
          zIndex: 100,
          overflow: 'auto',
          borderRight: `1px solid ${token.colorBorderSecondary}`,
        }}
      >
        <div
          style={{
            display: 'flex',
            alignItems: 'center',
            gap: 10,
            padding: collapsed ? '16px 0' : '16px 16px',
            justifyContent: collapsed ? 'center' : 'flex-start',
          }}
        >
          <div
            style={{
              width: 32,
              height: 32,
              flexShrink: 0,
              borderRadius: 8,
              background: `linear-gradient(135deg, ${token.colorPrimary}, ${mode === 'dark' ? '#6366f1' : '#7c3aed'})`,
              color: '#fff',
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'center',
              fontWeight: 800,
              fontSize: 13,
            }}
          >
            LS
          </div>
          {!collapsed && (
            <span style={{ fontWeight: 700, fontSize: 16, color: token.colorText }}>
              {t('app.title')}
            </span>
          )}
        </div>
        <Menu
          mode="inline"
          selectedKeys={[loc.pathname]}
          items={items}
          onClick={({ key }) => nav(key)}
          style={{ borderInlineEnd: 'none' }}
        />
      </Sider>

      {/* Content area offsets by the sider width with a smooth transition. */}
      <Layout style={{ marginLeft: collapsed ? SIDER_COLLAPSED_WIDTH : SIDER_WIDTH, transition: 'margin-left .2s' }}>
        <Header
          style={{
            position: 'sticky',
            top: 0,
            zIndex: 99,
            display: 'flex',
            justifyContent: 'flex-end',
            alignItems: 'center',
            gap: 16,
            paddingInline: 24,
            height: 56,
            lineHeight: '56px',
            background: token.colorBgContainer,
            borderBottom: `1px solid ${token.colorBorderSecondary}`,
          }}
        >
          <Space size={8} align="center">
            {/* Icon-only theme / language toggles (replaced the old
                Switch + Segmented pair). */}
            <Tooltip title={`${t('nav.theme')} · ${mode === 'dark' ? t('nav.themeDark') : t('nav.themeLight')}`}>
              <Button
                type="text"
                icon={mode === 'dark' ? <SunOutlined /> : <MoonOutlined />}
                onClick={toggle}
              />
            </Tooltip>
            <Tooltip title={`${t('nav.language')} · ${lang === 'en' ? 'EN' : '中文'}`}>
              <Button
                type="text"
                icon={<TranslationOutlined />}
                onClick={() => setLang(lang === 'en' ? 'zh' : 'en')}
              />
            </Tooltip>
            <Button icon={<LogoutOutlined />} onClick={logout}>
              {t('common.logout')}
            </Button>
          </Space>
        </Header>
        <Content style={{ margin: 16 }}>
          <Outlet />
        </Content>
      </Layout>
    </Layout>
  );
}
