import React from 'react';
import ReactDOM from 'react-dom/client';
import { App as AntApp, ConfigProvider, theme } from 'antd';
import { HashRouter } from 'react-router-dom';
import App from './App';
import { LangProvider, useLang } from './i18n/i18n';
import zhCN from 'antd/locale/zh_CN';
import enUS from 'antd/locale/en_US';

// Root shell reads the persisted language so antd's built-in component strings
// (pagination, date picker, popconfirm …) follow the same toggle.
function Shell() {
  const { lang } = useLang();
  return (
    <ConfigProvider
      locale={lang === 'zh' ? zhCN : enUS}
      theme={{ algorithm: theme.defaultAlgorithm }}
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
    <LangProvider>
      <Shell />
    </LangProvider>
  </React.StrictMode>,
);
