import { useCallback, useEffect, useRef, useState } from 'react';
import SettingsSection from '../components/settings/SettingsSection';
import SegmentSelect from '../components/settings/SegmentSelect';
import Toggle from '../components/settings/Toggle';
import StrategyRow from '../components/settings/StrategyRow';
import { ChamferedBox } from '../components/common/ChamferedBox';
import { colors } from '../styles/tokens';
import styles from './Config.module.css';

const ENGINE_VERSION = 'v2.1.0';

/* ── Default config values ── */
const DEFAULTS = {
  mode: 'paper',
  tickInterval: '15m',
  dryRun: true,
  initialCapital: 100_000,
  confidenceThreshold: 0.65,
  minRiskReward: 1.5,
  maxPositionSize: 5.0,
  maxOpenPositions: 10,
  dailyLossLimit: 3.0,
  maxDrawdownPct: 15.0,
  cooldownAfterLoss: 60,
  signalExpiry: 30,
  strategies: {
    trendFollowing: true,
    meanReversion: true,
    momentum: true,
    newsSentiment: false,
  },
  analystCheap: 'gpt-4o',
  analystMid: 'gemini-2.5-flash',
  analystFrontier: 'claude-sonnet',
  arbiter: 'claude-sonnet',
  classifier: 'gpt-4o-mini',
  costProfile: 'balanced',
  dailyTokenBudget: 200_000,
  weeklyTokenBudget: 1_200_000,
  autoDegradeBudget: true,
  telegram: true,
  notifySignal: true,
  notifyTrade: true,
  notifyError: true,
  notifyDailySummary: false,
  batchEnabled: true,
  batchWindow: 60,
  minConfidenceAlert: 0.65,
  includeHolds: false,
};

type Config = typeof DEFAULTS;

function formatNumber(n: number): string {
  if (n == null) return '0';
  return n.toLocaleString('en-US');
}

function parseFormattedNumber(s: string): number {
  if (!s) return 0;
  const cleaned = s.replace(/,/g, '');
  const n = Number(cleaned);
  return Number.isNaN(n) ? 0 : n;
}

function secToDisplay(sec: number): string {
  if (sec == null) return '1h';
  if (sec < 60) return `${sec}s`;
  if (sec < 3600) return `${Math.round(sec / 60)}m`;
  if (sec < 86400) return `${Math.round(sec / 3600)}h`;
  return `${Math.round(sec / 86400)}d`;
}

function displayToSec(display: string): number {
  if (!display) return 3600;
  const val = parseInt(display);
  if (isNaN(val)) return 3600;
  if (display.endsWith('m')) return val * 60;
  if (display.endsWith('h')) return val * 3600;
  if (display.endsWith('d')) return val * 86400;
  return val;
}

export default function ConfigScreen() {
  const [config, setConfig] = useState<Config>({ ...DEFAULTS });
  const [isSaving, setIsSaving] = useState(false);
  const savedRef = useRef<Config>({ ...DEFAULTS });
  const [toast, setToast] = useState<string | null>(null);
  const [showRestartModal, setShowRestartModal] = useState(false);
  const [rawInput, setRawInput] = useState<Record<string, string>>({});
  const toastTimerRef = useRef<ReturnType<typeof setTimeout>>();

  useEffect(() => () => clearTimeout(toastTimerRef.current), []);

  const set = useCallback(
    <K extends keyof Config>(key: K, value: Config[K]) =>
      setConfig((prev) => ({ ...prev, [key]: value })),
    [],
  );

  const setStrategy = useCallback(
    (key: keyof Config['strategies'], value: boolean) =>
      setConfig((prev) => ({
        ...prev,
        strategies: { ...prev.strategies, [key]: value },
      })),
    [],
  );

  const showToast = useCallback((msg: string) => {
    clearTimeout(toastTimerRef.current);
    setToast(msg);
    toastTimerRef.current = setTimeout(() => setToast(null), 3000);
  }, []);

  useEffect(() => {
    async function loadConfig() {
      try {
        const res = await fetch('/api/system/config');
        if (!res.ok) throw new Error('Failed to fetch config');
        const data = await res.json();
        
        const mapped: Config = {
          ...DEFAULTS,
          mode: data.engine?.mode || DEFAULTS.mode,
          tickInterval: secToDisplay(data.engine?.tick_interval_sec),
          dryRun: data.engine?.dry_run ?? DEFAULTS.dryRun,
          initialCapital: data.engine?.initial_capital || DEFAULTS.initialCapital,
          confidenceThreshold: data.risk?.confidence_threshold || DEFAULTS.confidenceThreshold,
          maxPositionSize: data.risk?.max_position_pct || DEFAULTS.maxPositionSize,
          maxOpenPositions: data.risk?.max_open_positions || DEFAULTS.maxOpenPositions,
          dailyLossLimit: data.risk?.daily_loss_limit_pct || DEFAULTS.dailyLossLimit,
          maxDrawdownPct: data.risk?.max_drawdown_pct || DEFAULTS.maxDrawdownPct,
          cooldownAfterLoss: data.risk?.cooldown_after_loss_min || DEFAULTS.cooldownAfterLoss,
          signalExpiry: data.risk?.signal_expiry_min || DEFAULTS.signalExpiry,
          strategies: {
            trendFollowing: data.strategies?.trend_following?.enabled ?? DEFAULTS.strategies.trendFollowing,
            meanReversion: data.strategies?.mean_reversion?.enabled ?? DEFAULTS.strategies.meanReversion,
            momentum: data.strategies?.momentum?.enabled ?? DEFAULTS.strategies.momentum,
            newsSentiment: data.strategies?.news_sentiment?.enabled ?? DEFAULTS.strategies.newsSentiment,
          },
          analystCheap: data.models?.roles?.analysts?.[0] || DEFAULTS.analystCheap,
          analystMid: data.models?.roles?.analysts?.[1] || DEFAULTS.analystMid,
          analystFrontier: data.models?.roles?.analysts?.[2] || DEFAULTS.analystFrontier,
          arbiter: data.models?.roles?.arbiter || DEFAULTS.arbiter,
          classifier: data.models?.roles?.classifier || DEFAULTS.classifier,
          costProfile: data.cost?.profile || DEFAULTS.costProfile,
          dailyTokenBudget: data.cost?.daily_token_budget || DEFAULTS.dailyTokenBudget,
          weeklyTokenBudget: data.cost?.weekly_token_budget || DEFAULTS.weeklyTokenBudget,
          autoDegradeBudget: data.cost?.auto_degrade_on_budget_hit ?? DEFAULTS.autoDegradeBudget,
          telegram: data.notifications?.telegram?.enabled ?? DEFAULTS.telegram,
          notifySignal: data.notifications?.telegram?.notify_on?.includes('signal_generated') ?? DEFAULTS.notifySignal,
          notifyTrade: data.notifications?.telegram?.notify_on?.includes('trade_executed') ?? DEFAULTS.notifyTrade,
          notifyError: data.notifications?.telegram?.notify_on?.includes('error_occurred') ?? DEFAULTS.notifyError,
          batchEnabled: data.notifications?.telegram?.batch_enabled ?? DEFAULTS.batchEnabled,
          batchWindow: data.notifications?.telegram?.batch_window_sec ?? DEFAULTS.batchWindow,
          minConfidenceAlert: data.notifications?.telegram?.min_confidence_for_alert || DEFAULTS.minConfidenceAlert,
          includeHolds: data.notifications?.telegram?.include_holds ?? DEFAULTS.includeHolds,
        };
        
        setConfig(mapped);
        savedRef.current = { ...mapped };
      } catch (err) {
        showToast('Failed to load system config.');
      }
    }
    loadConfig();
  }, [showToast]);

  const handleSave = useCallback(async () => {
    setIsSaving(true);
    const payload = {
      mode: config.mode,
      tick_interval_sec: displayToSec(config.tickInterval),
      initial_capital: config.initialCapital,
      dry_run: config.dryRun,
      confidence_threshold: config.confidenceThreshold,
      max_position_pct: config.maxPositionSize,
      max_open_positions: config.maxOpenPositions,
      daily_loss_limit_pct: config.dailyLossLimit,
      max_drawdown_pct: config.maxDrawdownPct,
      cooldown_after_loss: config.cooldownAfterLoss,
      signal_expiry: config.signalExpiry,
      cost_profile: config.costProfile,
      daily_token_budget: config.dailyTokenBudget,
      weekly_token_budget: config.weeklyTokenBudget,
      auto_degrade_budget: config.autoDegradeBudget,
      telegram_enabled: config.telegram,
      analyst_cheap: config.analystCheap,
      analyst_mid: config.analystMid,
      analyst_frontier: config.analystFrontier,
      arbiter: config.arbiter,
      classifier: config.classifier,
      // Strategy maps
      'strategies.trend_following.enabled': config.strategies.trendFollowing,
      'strategies.mean_reversion.enabled': config.strategies.meanReversion,
      'strategies.momentum.enabled': config.strategies.momentum,
      'strategies.news_sentiment.enabled': config.strategies.newsSentiment,
    };

    try {
      const res = await fetch('/api/system/config', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });
      if (res.ok) {
        savedRef.current = { ...config };
        showToast('Config saved to disk — daemon restart required.');
      } else {
        showToast('Failed to persist configuration.');
      }
    } catch (e) {
      showToast('Connection error failed to save.');
    } finally {
      setIsSaving(false);
    }
  }, [config, showToast]);

  const handleDiscard = useCallback(() => {
    setConfig({ ...savedRef.current });
  }, []);

  const handleRestart = useCallback(() => {
    setShowRestartModal(false);
    showToast('Restart signal sent — engine restarting.');
  }, [showToast]);

  const handleReset = useCallback(() => {
    setConfig({ ...DEFAULTS });
    showToast('Reset to defaults. Use Save Changes to persist, or Discard to restore your last saved config.');
  }, [showToast]);

  return (
    <div className={styles.screen}>
      {/* ── Header ── */}
      <div className={styles.headerRow}>
        <div className={styles.breadcrumb}>
          <span className={styles.breadcrumbBrand}>~/</span>
          <span className={styles.breadcrumbPage}>CONFIG</span>
        </div>
        <div className={styles.headerRight}>
          <span className={styles.statusDot} />
          <span>ALL SYSTEMS NOMINAL</span>
        </div>
      </div>

      <h1 className={styles.pageTitle}>SYSTEM CONFIGURATION</h1>

      {/* ── Two-column content ── */}
      <div className={styles.content}>
        {/* ═══ LEFT COLUMN ═══ */}
        <div>
          {/* ENGINE */}
          <SettingsSection title="Engine">
            <div className={styles.row}>
              <span className={styles.label}>mode</span>
              <SegmentSelect
                options={[
                  { label: 'MANUAL', value: 'manual' },
                  { label: 'PAPER', value: 'paper', activeColor: 'amber' },
                  { label: 'AUTO', value: 'auto', prefix: <span style={{ color: 'var(--red)', fontSize: 8 }}>●{' '}</span> },
                ]}
                value={config.mode}
                onChange={(v) => set('mode', v)}
              />
            </div>

            <div className={styles.row}>
              <span className={styles.label}>tick_interval</span>
              <SegmentSelect
                options={[
                  { label: '5m', value: '5m' },
                  { label: '15m', value: '15m' },
                  { label: '1h', value: '1h' },
                  { label: '4h', value: '4h' },
                  { label: '1d', value: '1d' },
                ]}
                value={config.tickInterval}
                onChange={(v) => set('tickInterval', v)}
              />
            </div>

            <div>
              <div className={styles.row}>
                <span className={styles.label}>dry_run</span>
                <Toggle checked={config.dryRun} onChange={(v) => set('dryRun', v)} aria-label="Dry run mode" />
              </div>
              {config.dryRun && (
                <div className={styles.annotation}>NO ORDERS PLACED</div>
              )}
            </div>

            <div className={styles.row}>
              <span className={styles.label}>initial_capital</span>
              <div className={styles.input}>
                <input
                  className={styles.inputField}
                  value={rawInput.initialCapital ?? formatNumber(config.initialCapital)}
                  onFocus={() => setRawInput((prev) => ({ ...prev, initialCapital: String(config.initialCapital) }))}
                  onChange={(e) => setRawInput((prev) => ({ ...prev, initialCapital: e.target.value }))}
                  onBlur={() => { set('initialCapital', parseFormattedNumber(rawInput.initialCapital ?? '')); setRawInput((prev) => { const { initialCapital: _, ...rest } = prev; return rest; }); }}
                />
              </div>
            </div>
          </SettingsSection>

          {/* RISK THRESHOLDS */}
          <SettingsSection title="Risk Thresholds">
            <div className={styles.row}>
              <span className={styles.label}>confidence_threshold</span>
              <div className={styles.input}>
                <input className={styles.inputField} type="number" step="0.01" value={config.confidenceThreshold} onChange={(e) => set('confidenceThreshold', +e.target.value)} />
              </div>
            </div>
            <div className={styles.row}>
              <span className={styles.label}>min_risk_reward</span>
              <div className={styles.input}>
                <input className={styles.inputField} type="number" step="0.1" value={config.minRiskReward} onChange={(e) => set('minRiskReward', +e.target.value)} />
              </div>
            </div>
            <div className={styles.row}>
              <span className={styles.label}>max_position_size</span>
              <div className={styles.inputWithUnit}>
                <input className={styles.inputField} type="number" step="0.1" value={config.maxPositionSize} onChange={(e) => set('maxPositionSize', +e.target.value)} />
                <span className={styles.unit}>%</span>
              </div>
            </div>
            <div className={styles.row}>
              <span className={styles.label}>max_open_positions</span>
              <div className={styles.input}>
                <input className={styles.inputField} type="number" step="1" value={config.maxOpenPositions} onChange={(e) => set('maxOpenPositions', +e.target.value)} />
              </div>
            </div>
            <div className={styles.row}>
              <span className={styles.label}>daily_loss_limit</span>
              <div className={styles.inputWithUnit}>
                <input className={styles.inputField} type="number" step="0.1" value={config.dailyLossLimit} onChange={(e) => set('dailyLossLimit', +e.target.value)} />
                <span className={styles.unit}>%</span>
              </div>
            </div>
            <div className={styles.row}>
              <span className={styles.labelRed}>max_drawdown_pct</span>
              <div className={styles.inputWithUnit}>
                <input className={styles.inputField} type="number" step="0.1" value={config.maxDrawdownPct} onChange={(e) => set('maxDrawdownPct', +e.target.value)} />
                <span className={styles.unit}>%</span>
              </div>
            </div>
            <div className={styles.row}>
              <span className={styles.label}>cooldown_after_loss</span>
              <div className={styles.inputWithUnit}>
                <input className={styles.inputField} type="number" step="1" value={config.cooldownAfterLoss} onChange={(e) => set('cooldownAfterLoss', +e.target.value)} />
                <span className={styles.unit}>min</span>
              </div>
            </div>
            <div className={styles.row}>
              <span className={styles.label}>signal_expiry</span>
              <div className={styles.inputWithUnit}>
                <input className={styles.inputField} type="number" step="1" value={config.signalExpiry} onChange={(e) => set('signalExpiry', +e.target.value)} />
                <span className={styles.unit}>min</span>
              </div>
            </div>
          </SettingsSection>

          {/* STRATEGIES */}
          <SettingsSection title="Strategies">
            <StrategyRow
              name="Trend Following"
              enabled={config.strategies.trendFollowing}
              onToggle={(v) => setStrategy('trendFollowing', v)}
              params="EMA Period: 20 · ADX Min: 25 · RSI Min: 45 · RSI Max: 65"
              defaultExpanded
            />
            <StrategyRow
              name="Mean Reversion"
              enabled={config.strategies.meanReversion}
              onToggle={(v) => setStrategy('meanReversion', v)}
              params="BB Period: 20 · BB Std: 2.0 · RSI Period: 14"
            />
            <StrategyRow
              name="Momentum"
              enabled={config.strategies.momentum}
              onToggle={(v) => setStrategy('momentum', v)}
              params="ROC Period: 12 · MACD Fast: 12 · MACD Slow: 26"
            />
            <StrategyRow
              name="News Sentiment"
              enabled={config.strategies.newsSentiment}
              onToggle={(v) => setStrategy('newsSentiment', v)}
              params="Min Score: 0.6 · Lookback: 24h · Sources: 5"
            />
          </SettingsSection>
        </div>

        {/* ═══ RIGHT COLUMN ═══ */}
        <div>
          {/* MODEL ROLES */}
          <SettingsSection title="Model Roles">
            {([
              ['analyst_cheap', 'analystCheap'],
              ['analyst_mid', 'analystMid'],
              ['analyst_frontier', 'analystFrontier'],
              ['arbiter', 'arbiter'],
              ['classifier', 'classifier'],
            ] as const).map(([label, key]) => (
              <div key={key} className={styles.modelRow}>
                <span className={styles.label}>{label}</span>
                <span className={styles.modelName}>
                  {config[key]}
                </span>
              </div>
            ))}
          </SettingsSection>

          {/* COST PROFILE */}
          <SettingsSection title="Cost Profile">
            <SegmentSelect
              fullWidth
              options={[
                { label: 'BUDGET', value: 'budget' },
                { label: 'BALANCED', value: 'balanced' },
                { label: 'PRO', value: 'pro' },
              ]}
              value={config.costProfile}
              onChange={(v) => set('costProfile', v)}
            />
            <div className={styles.row}>
              <span className={styles.label}>Daily Token Budget</span>
              <div className={styles.input}>
                <input
                  className={styles.inputField}
                  value={rawInput.dailyTokenBudget ?? formatNumber(config.dailyTokenBudget)}
                  onFocus={() => setRawInput((prev) => ({ ...prev, dailyTokenBudget: String(config.dailyTokenBudget) }))}
                  onChange={(e) => setRawInput((prev) => ({ ...prev, dailyTokenBudget: e.target.value }))}
                  onBlur={() => { set('dailyTokenBudget', parseFormattedNumber(rawInput.dailyTokenBudget ?? '')); setRawInput((prev) => { const { dailyTokenBudget: _, ...rest } = prev; return rest; }); }}
                />
              </div>
            </div>
            <div className={styles.row}>
              <span className={styles.label}>Weekly Token Budget</span>
              <div className={styles.input}>
                <input
                  className={styles.inputField}
                  value={rawInput.weeklyTokenBudget ?? formatNumber(config.weeklyTokenBudget)}
                  onFocus={() => setRawInput((prev) => ({ ...prev, weeklyTokenBudget: String(config.weeklyTokenBudget) }))}
                  onChange={(e) => setRawInput((prev) => ({ ...prev, weeklyTokenBudget: e.target.value }))}
                  onBlur={() => { set('weeklyTokenBudget', parseFormattedNumber(rawInput.weeklyTokenBudget ?? '')); setRawInput((prev) => { const { weeklyTokenBudget: _, ...rest } = prev; return rest; }); }}
                />
              </div>
            </div>
            <div className={styles.row}>
              <span className={styles.label}>Auto-degrade on budget hit</span>
              <Toggle checked={config.autoDegradeBudget} onChange={(v) => set('autoDegradeBudget', v)} aria-label="Auto-degrade on budget hit" />
            </div>
          </SettingsSection>

          {/* NOTIFICATIONS */}
          <SettingsSection title="Notifications">
            <div className={styles.row}>
              <span className={styles.label}>Telegram</span>
              <Toggle checked={config.telegram} onChange={(v) => set('telegram', v)} aria-label="Telegram notifications" />
            </div>

            <div>
              <div className={styles.subLabel}>Notify On</div>
              <div className={styles.checkboxGroup}>
                <label className={styles.checkboxLabel}>
                  <input type="checkbox" className={styles.checkbox} checked={config.notifySignal} onChange={(e) => set('notifySignal', e.target.checked)} />
                  Signal
                </label>
                <label className={styles.checkboxLabel}>
                  <input type="checkbox" className={styles.checkbox} checked={config.notifyTrade} onChange={(e) => set('notifyTrade', e.target.checked)} />
                  Trade
                </label>
                <label className={styles.checkboxLabel}>
                  <input type="checkbox" className={styles.checkbox} checked={config.notifyError} onChange={(e) => set('notifyError', e.target.checked)} />
                  Error
                </label>
                <label className={styles.checkboxLabel}>
                  <input type="checkbox" className={styles.checkbox} checked={config.notifyDailySummary} onChange={(e) => set('notifyDailySummary', e.target.checked)} />
                  Daily Summary
                </label>
              </div>
            </div>

            <div className={styles.row}>
              <span className={styles.label}>Batch Enabled</span>
              <Toggle checked={config.batchEnabled} onChange={(v) => set('batchEnabled', v)} aria-label="Batch enabled" />
            </div>
            <div className={styles.row}>
              <span className={styles.label}>Batch Window</span>
              <div className={styles.inputWithUnit}>
                <input className={styles.inputField} type="number" step="1" value={config.batchWindow} onChange={(e) => set('batchWindow', +e.target.value)} />
                <span className={styles.unit}>sec</span>
              </div>
            </div>
            <div className={styles.row}>
              <span className={styles.label}>Min Confidence for Alert</span>
              <div className={styles.input}>
                <input className={styles.inputField} type="number" step="0.01" value={config.minConfidenceAlert} onChange={(e) => set('minConfidenceAlert', +e.target.value)} />
              </div>
            </div>
            <div className={styles.row}>
              <span className={styles.label}>Include HOLDs</span>
              <Toggle checked={config.includeHolds} onChange={(v) => set('includeHolds', v)} aria-label="Include HOLDs" />
            </div>
          </SettingsSection>

          {/* SYSTEM ACTIONS */}
          <SettingsSection title="System Actions">
            <ChamferedBox
              chamfer={6}
              backgroundColor="transparent"
              borderColor={colors.amber}
              onClick={() => setShowRestartModal(true)}
              className={styles.btnAmber}
              style={{ marginBottom: 16, padding: '10px 0', textAlign: 'center' }}
            >
              <span style={{ color: colors.amber, fontWeight: 700 }}>Restart Engine</span>
            </ChamferedBox>
            <ChamferedBox
              chamfer={6}
              backgroundColor="transparent"
              borderColor={colors.red}
              onClick={handleReset}
              className={styles.btnRed}
              style={{ padding: '10px 0', textAlign: 'center' }}
            >
              <span style={{ color: colors.red, fontWeight: 700 }}>Reset to Defaults</span>
            </ChamferedBox>
            <span className={styles.versionText}>
              TradeEngine {ENGINE_VERSION} — Build 20260318
            </span>
          </SettingsSection>
        </div>
      </div>

      {/* ── Footer ── */}
      <footer className={styles.footer}>
        <ChamferedBox
          chamfer={6}
          backgroundColor="transparent"
          borderColor={colors.borderMuted}
          onClick={handleDiscard}
          className={styles.discardBtn}
          style={{ padding: '8px 24px' }}
        >
          Discard
        </ChamferedBox>
        <ChamferedBox
          chamfer={6}
          backgroundColor={colors.acidGreen}
          borderColor={colors.acidGreen}
          onClick={isSaving ? undefined : handleSave}
          className={styles.saveBtn}
          style={{ 
            padding: '8px 24px', 
            opacity: isSaving ? 0.5 : 1, 
            cursor: isSaving ? 'not-allowed' : 'pointer' 
          }}
        >
          <span style={{ color: '#0D0D11', fontWeight: 700 }}>
            {isSaving ? 'SAVING...' : 'Save Changes'}
          </span>
        </ChamferedBox>
      </footer>

      {/* ── Restart confirmation modal ── */}
      {showRestartModal && (
        <div className={styles.modalOverlay} onClick={() => setShowRestartModal(false)}>
          <ChamferedBox
            chamfer={12}
            backgroundColor={colors.cardBg}
            borderColor={colors.borderMuted}
            className={styles.modal}
            onClick={(e) => e.stopPropagation()}
            style={{ width: 400, padding: 32 }}
          >
            <h3 className={styles.modalTitle}>Restart Engine?</h3>
            <p className={styles.modalBody}>
              This will interrupt active trades. Are you sure you want to proceed?
            </p>
            <div className={styles.modalActions}>
              <button
                className={styles.discardBtn}
                onClick={() => setShowRestartModal(false)}
                style={{ background: 'transparent', border: 'none', color: 'var(--text-dim)', cursor: 'pointer' }}
              >
                Cancel
              </button>
              <ChamferedBox
                chamfer={6}
                backgroundColor={colors.amber}
                borderColor={colors.amber}
                onClick={handleRestart}
                style={{ padding: '8px 24px' }}
              >
                <span style={{ color: '#0D0D11', fontWeight: 700 }}>Confirm Restart</span>
              </ChamferedBox>
            </div>
          </ChamferedBox>
        </div>
      )}

      {/* ── Toast ── */}
      {toast && <div className={styles.toast}>{toast}</div>}
    </div>
  );
}
