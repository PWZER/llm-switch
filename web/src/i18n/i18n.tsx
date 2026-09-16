import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from 'react';
import { en, zh } from './dictionaries';

export type Lang = 'en' | 'zh';

const LANG_KEY = 'lsw_lang';

function initialLang(): Lang {
  const stored = localStorage.getItem(LANG_KEY);
  if (stored === 'zh' || stored === 'en') return stored;
  // Default to the browser language when it is Chinese.
  return navigator.language.toLowerCase().startsWith('zh') ? 'zh' : 'en';
}

interface LangCtx {
  lang: Lang;
  setLang: (l: Lang) => void;
  t: (key: string) => string;
}

const Ctx = createContext<LangCtx>({
  lang: 'en',
  setLang: () => {},
  t: (k) => k,
});

const dicts: Record<Lang, Record<string, string>> = { en, zh };

export function LangProvider({ children }: { children: ReactNode }) {
  const [lang, setLangState] = useState<Lang>(initialLang);

  const setLang = useCallback((l: Lang) => {
    setLangState(l);
    localStorage.setItem(LANG_KEY, l);
  }, []);

  const t = useCallback(
    (key: string) => dicts[lang][key] ?? en[key] ?? key,
    [lang],
  );

  const value = useMemo(() => ({ lang, setLang, t }), [lang, setLang, t]);
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useLang(): LangCtx {
  return useContext(Ctx);
}
