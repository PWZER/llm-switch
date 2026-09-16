import React from 'react';
import ReactDOM from 'react-dom/client';
import { App as AntApp, ConfigProvider, theme } from 'antd';
import { HashRouter } from 'react-router-dom';
import App from './App';
import { LangProvider, useLang } from './i18n/i18n';
import { ThemeProvider, useTheme } from './theme/theme';
import zhCN from 'antd/locale/zh_CN';
import enUS from 'antd/locale/en_US';

// Brand identity: indigo primary instead of antd default blue.
const brand = {
  colorPrimary: '#4f46e5',
  borderRadius: 6,
};

// Root shell reads the persisted theme + language so antd's algorithms and
// built-in component strings follow the header toggles.
function Shell() {
  const { mode } = useTheme();
  const { lang } = useLang();
  return (
    <ConfigProvider
      locale={lang === 'zh' ? zhCN : enUS}
      theme={{
        algorithm: mode === 'dark' ? theme.darkAlgorithm : theme.defaultAlgorithm,
        token: mode === 'dark' ? { ...brand, colorPrimary: '#818cf8' } : brand,
      }}
    >
      <AntApp>
        <HashRouter>
          <App />
        </HashRouter>
      </AntApp>
    </ConfigProvider>
  );
}

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <ThemeProvider>
      <LangProvider>
        <Shell />
      </LangProvider>
    </ThemeProvider>
  </React.StrictMode>,
);
