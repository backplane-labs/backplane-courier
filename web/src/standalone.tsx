import 'virtual:backplane-grafana-assets';
import { useLayoutEffect, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { BrowserRouter, Routes, Route, NavLink } from 'react-router-dom';
import i18next from 'i18next';
import { initReactI18next, I18nextProvider, useTranslation } from 'react-i18next';
import { ThemeContext } from '@grafana/ui';
import { createGrafanaTheme, type ThemeMode } from '@backplane-labs/backplane-theme';
import { BackplaneProvider } from '@backplane-labs/backplane-react';
import { PluginProvider, installPluginTranslations } from '@backplane-labs/backplane-plugin-sdk';
import { Badge, Button, englishResources } from '@backplane-labs/backplane-ui';
import { fixtureClient } from './fixtures';
import navigation from './navigation';
import Embedded from './embedded';
import '@backplane-labs/backplane-ui/style.css';
import '@backplane-labs/backplane-theme/style.css';
import '@fontsource/ibm-plex-sans/400.css';
import '@fontsource/ibm-plex-sans/500.css';
import '@fontsource/ibm-plex-sans/600.css';
import '@fontsource/ibm-plex-mono/400.css';
import './standalone.css';

const i18n = i18next.createInstance();
await i18n.use(initReactI18next).init({ lng: 'en', fallbackLng: 'en', supportedLngs: ['en'], interpolation: { escapeValue: false }, resources: { en: { 'backplane.ui': englishResources } } });
installPluginTranslations(i18n, navigation);
function Standalone() {
  const { t } = useTranslation('module.courier');
  const [mode, setMode] = useState<ThemeMode>('dark');
  useLayoutEffect(() => { document.documentElement.dataset.theme = mode; document.documentElement.classList.toggle('dark', mode === 'dark'); }, [mode]);
  return <ThemeContext.Provider value={createGrafanaTheme(mode)}><BackplaneProvider client={fixtureClient}>
    <PluginProvider context={{ service: 'courier', basePath: '/', mode, environment: 'standalone' }}>
      <header><span className="module-brand">{navigation.service}<Badge variant="outline">{t('standalone')}</Badge></span><nav>{navigation.nav.map((item) => <NavLink key={item.path} end to={`/${item.path}`}>{t(item.labelKey)}</NavLink>)}</nav><Button variant="outline" size="sm" onClick={() => setMode(mode === 'dark' ? 'light' : 'dark')}>{t(mode === 'dark' ? 'light' : 'dark')}</Button></header>
      <main><Routes><Route path="/*" element={<Embedded />} /></Routes></main>
    </PluginProvider>
  </BackplaneProvider></ThemeContext.Provider>;
}
createRoot(document.getElementById('root')!).render(<I18nextProvider i18n={i18n}><BrowserRouter><Standalone /></BrowserRouter></I18nextProvider>);
