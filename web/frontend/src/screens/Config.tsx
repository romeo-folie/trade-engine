import { useCallback, useEffect, useRef, useState } from 'react';
import SettingsSection from '../components/settings/SettingsSection';
import SegmentSelect from '../components/settings/SegmentSelect';
import Toggle from '../components/settings/Toggle';
import StrategyRow from '../components/settings/StrategyRow';
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
  return n.toLocaleString('en-US');
}

function parseFormattedNumber(s: string): number {
  const cleaned = s.replace(/,/g, '');
  const n = Number(cleaned);
  return Number.isNaN(n) ? 0 : n;
}

export default function ConfigScreen() {
  const [config, setConfig] = useState<Config>({ ...DEFAULTS });
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

  const handleSave = useCallback(() => {
    savedRef.current = { ...config };
    showToast('Config saved locally — daemon restart required.');
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
      <header className={styles.header}>
        <span className={styles.headerTitle}>TradeEngine</span>
        <span className={styles.headerSub}>System Configuration</span>
        <div className={styles.headerRight}>
          <span>{ENGINE_VERSION}</span>
          <span className={styles.statusDot} />
          <span>ALL SYSTEMS NOMINAL</span>
        </div>
      </header>

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
                  { label: 'PAPER', value: 'paper' },
                  { label: 'AUTO', value: 'auto', prefix: <span style={{ color: 'var(--amber)' }}>●{' '}</span> },
                ]}
                value={config.mode}
                onChange={(v) => set('mode', v)}
                color="emerald"
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
              <input
                className={styles.input}
                value={rawInput.initialCapital ?? formatNumber(config.initialCapital)}
                onFocus={() => setRawInput((prev) => ({ ...prev, initialCapital: String(config.initialCapital) }))}
                onChange={(e) => setRawInput((prev) => ({ ...prev, initialCapital: e.target.value }))}
                onBlur={() => { set('initialCapital', parseFormattedNumber(rawInput.initialCapital ?? '')); setRawInput((prev) => { const { initialCapital: _, ...rest } = prev; return rest; }); }}
              />
            </div>
          </SettingsSection>

          {/* RISK THRESHOLDS */}
          <SettingsSection title="Risk Thresholds">
            <div className={styles.row}>
              <span className={styles.label}>confidence_threshold</span>
              <input className={styles.input} type="number" step="0.01" value={config.confidenceThreshold} onChange={(e) => set('confidenceThreshold', +e.target.value)} />
            </div>
            <div className={styles.row}>
              <span className={styles.label}>min_risk_reward</span>
              <input className={styles.input} type="number" step="0.1" value={config.minRiskReward} onChange={(e) => set('minRiskReward', +e.target.value)} />
            </div>
            <div className={styles.row}>
              <span className={styles.label}>max_position_size</span>
              <div className={styles.inputWithUnit}>
                <input className={styles.input} type="number" step="0.1" value={config.maxPositionSize} onChange={(e) => set('maxPositionSize', +e.target.value)} />
                <span className={styles.unit}>%</span>
              </div>
            </div>
            <div className={styles.row}>
              <span className={styles.label}>max_open_positions</span>
              <input className={styles.input} type="number" step="1" value={config.maxOpenPositions} onChange={(e) => set('maxOpenPositions', +e.target.value)} />
            </div>
            <div className={styles.row}>
              <span className={styles.label}>daily_loss_limit</span>
              <div className={styles.inputWithUnit}>
                <input className={styles.input} type="number" step="0.1" value={config.dailyLossLimit} onChange={(e) => set('dailyLossLimit', +e.target.value)} />
                <span className={styles.unit}>%</span>
              </div>
            </div>
            <div className={styles.row}>
              <span className={styles.labelAmber}>max_drawdown_pct</span>
              <div className={styles.inputWithUnit}>
                <input className={styles.input} type="number" step="0.1" value={config.maxDrawdownPct} onChange={(e) => set('maxDrawdownPct', +e.target.value)} />
                <span className={styles.unit}>%</span>
              </div>
            </div>
            <div className={styles.row}>
              <span className={styles.label}>cooldown_after_loss</span>
              <div className={styles.inputWithUnit}>
                <input className={styles.input} type="number" step="1" value={config.cooldownAfterLoss} onChange={(e) => set('cooldownAfterLoss', +e.target.value)} />
                <span className={styles.unit}>min</span>
              </div>
            </div>
            <div className={styles.row}>
              <span className={styles.label}>signal_expiry</span>
              <div className={styles.inputWithUnit}>
                <input className={styles.input} type="number" step="1" value={config.signalExpiry} onChange={(e) => set('signalExpiry', +e.target.value)} />
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
                  <button type="button" className={styles.swapIcon} onClick={() => { /* TODO: open model selector modal */ }}>⇄</button>
                </span>
              </div>
            ))}
          </SettingsSection>

          {/* COST PROFILE */}
          <SettingsSection title="Cost Profile">
            <SegmentSelect
              options={[
                { label: 'BUDGET', value: 'budget' },
                { label: 'BALANCED', value: 'balanced' },
                { label: 'PRO', value: 'pro' },
              ]}
              value={config.costProfile}
              onChange={(v) => set('costProfile', v)}
              color="emerald"
            />
            <div className={styles.row}>
              <span className={styles.label}>Daily Token Budget</span>
              <input
                className={styles.input}
                value={rawInput.dailyTokenBudget ?? formatNumber(config.dailyTokenBudget)}
                onFocus={() => setRawInput((prev) => ({ ...prev, dailyTokenBudget: String(config.dailyTokenBudget) }))}
                onChange={(e) => setRawInput((prev) => ({ ...prev, dailyTokenBudget: e.target.value }))}
                onBlur={() => { set('dailyTokenBudget', parseFormattedNumber(rawInput.dailyTokenBudget ?? '')); setRawInput((prev) => { const { dailyTokenBudget: _, ...rest } = prev; return rest; }); }}
              />
            </div>
            <div className={styles.row}>
              <span className={styles.label}>Weekly Token Budget</span>
              <input
                className={styles.input}
                value={rawInput.weeklyTokenBudget ?? formatNumber(config.weeklyTokenBudget)}
                onFocus={() => setRawInput((prev) => ({ ...prev, weeklyTokenBudget: String(config.weeklyTokenBudget) }))}
                onChange={(e) => setRawInput((prev) => ({ ...prev, weeklyTokenBudget: e.target.value }))}
                onBlur={() => { set('weeklyTokenBudget', parseFormattedNumber(rawInput.weeklyTokenBudget ?? '')); setRawInput((prev) => { const { weeklyTokenBudget: _, ...rest } = prev; return rest; }); }}
              />
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
                <input className={styles.input} type="number" step="1" value={config.batchWindow} onChange={(e) => set('batchWindow', +e.target.value)} />
                <span className={styles.unit}>sec</span>
              </div>
            </div>
            <div className={styles.row}>
              <span className={styles.label}>Min Confidence for Alert</span>
              <input className={styles.input} type="number" step="0.01" value={config.minConfidenceAlert} onChange={(e) => set('minConfidenceAlert', +e.target.value)} />
            </div>
            <div className={styles.row}>
              <span className={styles.label}>Include HOLDs</span>
              <Toggle checked={config.includeHolds} onChange={(v) => set('includeHolds', v)} aria-label="Include HOLDs" />
            </div>
          </SettingsSection>

          {/* SYSTEM ACTIONS */}
          <SettingsSection title="System Actions">
            <button
              className={`${styles.btnAmber} chamfer-sm`}
              onClick={() => setShowRestartModal(true)}
            >
              Restart Engine
            </button>
            <button
              className={`${styles.btnRed} chamfer-sm`}
              onClick={handleReset}
            >
              Reset to Defaults
            </button>
            <span className={styles.versionText}>
              TradeEngine {ENGINE_VERSION} — Build 20260318
            </span>
          </SettingsSection>
        </div>
      </div>

      {/* ── Footer ── */}
      <footer className={styles.footer}>
        <button className={`${styles.discardBtn} chamfer-sm`} onClick={handleDiscard}>
          Discard
        </button>
        <button className={`${styles.saveBtn} chamfer-sm`} onClick={handleSave}>
          Save Changes
        </button>
      </footer>

      {/* ── Restart confirmation modal ── */}
      {showRestartModal && (
        <div className={styles.modalOverlay} onClick={() => setShowRestartModal(false)}>
          <div className={`${styles.modal} chamfer`} onClick={(e) => e.stopPropagation()}>
            <h3 className={styles.modalTitle}>Restart Engine?</h3>
            <p className={styles.modalBody}>
              This will interrupt active trades. Are you sure you want to proceed?
            </p>
            <div className={styles.modalActions}>
              <button
                className={`${styles.discardBtn} chamfer-sm`}
                onClick={() => setShowRestartModal(false)}
              >
                Cancel
              </button>
              <button
                className={`${styles.btnAmber} chamfer-sm`}
                onClick={handleRestart}
              >
                Confirm Restart
              </button>
            </div>
          </div>
        </div>
      )}

      {/* ── Toast ── */}
      {toast && <div className={styles.toast}>{toast}</div>}
    </div>
  );
}
