import { useState } from 'react';
import styles from './Config.module.css';

/* ── Tiny helpers ─────────────────────────────────────────────────── */

function cls(...classes: (string | false | undefined)[]) {
  return classes.filter(Boolean).join(' ');
}

function Toggle({ on, onToggle }: { on: boolean; onToggle: () => void }) {
  return (
    <button
      type="button"
      className={cls(styles.toggleSwitch, on && styles.toggleSwitchOn)}
      onClick={onToggle}
    />
  );
}

function Checkbox({ on, onToggle, label }: { on: boolean; onToggle: () => void; label: string }) {
  return (
    <div className={styles.checkboxRow}>
      <div
        className={cls(styles.checkbox, on && styles.checkboxOn)}
        onClick={onToggle}
      >
        {on ? '✓' : ''}
      </div>
      <span className={styles.checkboxLabel}>{label}</span>
    </div>
  );
}

/* ── Strategy config (expandable) ─────────────────────────────────── */

interface StrategyDef {
  name: string;
  params: { label: string; value: string }[];
}

const STRATEGIES: StrategyDef[] = [
  { name: 'Trend Following', params: [{ label: 'lookback_period', value: '20' }, { label: 'atr_multiplier', value: '2.5' }] },
  { name: 'Mean Reversion',  params: [{ label: 'z_score_threshold', value: '2.0' }, { label: 'lookback_period', value: '14' }] },
  { name: 'Momentum',        params: [{ label: 'rsi_period', value: '14' }, { label: 'rsi_overbought', value: '70' }, { label: 'rsi_oversold', value: '30' }] },
  { name: 'News Sentiment',  params: [{ label: 'min_sentiment_score', value: '0.6' }, { label: 'decay_hours', value: '24' }] },
];

/* ── Model roles ──────────────────────────────────────────────────── */

const MODEL_ROLES = [
  { role: 'Technical Analyst',  model: 'gpt-4o' },
  { role: 'Momentum Analyst',   model: 'claude-sonnet-4-5' },
  { role: 'Sentiment Analyst',  model: 'gemini-flash' },
  { role: 'Arbiter',            model: 'gpt-4o' },
  { role: 'Classifier',         model: 'claude-sonnet-4-5' },
];

/* ── Component ────────────────────────────────────────────────────── */

export default function ConfigScreen() {
  /* Engine */
  const [mode, setMode] = useState<'MANUAL' | 'PAPER' | 'AUTO'>('PAPER');
  const [tick, setTick] = useState('15m');
  const [dryRun, setDryRun] = useState(true);
  const [initialCapital, setInitialCapital] = useState('100000');

  /* Risk */
  const [confidence, setConfidence] = useState('0.7');
  const [minRiskReward, setMinRiskReward] = useState('2.0');
  const [maxPosPct, setMaxPosPct] = useState('5');
  const [maxOpen, setMaxOpen] = useState('5');
  const [dailyLoss, setDailyLoss] = useState('3');
  const [maxDrawdown, setMaxDrawdown] = useState('8');
  const [cooldown, setCooldown] = useState('30');
  const [signalExpiry, setSignalExpiry] = useState('60');

  /* Strategies */
  const [strategyOn, setStrategyOn] = useState<Record<string, boolean>>({
    'Trend Following': true, 'Mean Reversion': true, 'Momentum': false, 'News Sentiment': true,
  });
  const [expandedStrategy, setExpandedStrategy] = useState<string | null>(null);

  /* Cost */
  const [costProfile, setCostProfile] = useState('BALANCED');
  const [dailyTokens, setDailyTokens] = useState('500000');
  const [weeklyTokens, setWeeklyTokens] = useState('3000000');
  const [autoDegrade, setAutoDegrade] = useState(true);

  /* Notifications */
  const [telegram, setTelegram] = useState(true);
  const [notifyChecks, setNotifyChecks] = useState<Record<string, boolean>>({
    'New signal': true, 'Trade executed': true, 'Take-profit hit': true,
    'Stop-loss hit': true, 'Daily summary': false,
  });
  const [batchEnabled, setBatchEnabled] = useState(false);
  const [batchWindow, setBatchWindow] = useState('15');
  const [minConfAlert, setMinConfAlert] = useState('0.6');
  const [includeHolds, setIncludeHolds] = useState(false);

  /* ── Render helpers ─────────────────────────────────────────────── */

  const modeStyle = (m: string) => {
    if (m !== mode) return styles.toggleBtn;
    if (m === 'MANUAL') return cls(styles.toggleBtn, styles.toggleBtnActiveGreen);
    if (m === 'PAPER')  return cls(styles.toggleBtn, styles.toggleBtnActiveAmber);
    return cls(styles.toggleBtn, styles.toggleBtnActiveRed);
  };

  const segmentStyle = (val: string, active: string) =>
    cls(styles.toggleBtn, val === active && styles.toggleBtnActiveGreen);

  return (
    <div className={styles.screen}>
      <div className={styles.columns}>
        {/* ════════════ LEFT COLUMN ════════════ */}
        <div>
          {/* Engine */}
          <div className={styles.section}>
            <div className={styles.sectionHeader}>Engine</div>

            <div className={styles.toggleGroup}>
              {(['MANUAL', 'PAPER', 'AUTO'] as const).map(m => (
                <button key={m} className={modeStyle(m)} onClick={() => setMode(m)}>
                  {m === 'AUTO' && mode === 'AUTO' && <span className={styles.warningDot} />}
                  {m}
                </button>
              ))}
            </div>

            <div className={styles.toggleGroup}>
              {['5m', '15m', '1h', '4h', '1d'].map(t => (
                <button key={t} className={segmentStyle(t, tick)} onClick={() => setTick(t)}>
                  {t}
                </button>
              ))}
            </div>

            <div className={styles.fieldRow}>
              <span className={styles.fieldLabel}>
                Dry Run
                {dryRun && <span className={styles.warningBadge}>NO ORDERS PLACED</span>}
              </span>
              <Toggle on={dryRun} onToggle={() => setDryRun(!dryRun)} />
            </div>

            <div className={styles.fieldRow}>
              <span className={styles.fieldLabel}>Initial Capital</span>
              <input
                className={styles.fieldInput}
                type="number"
                value={initialCapital}
                onChange={e => setInitialCapital(e.target.value)}
              />
            </div>
          </div>

          {/* Risk Thresholds */}
          <div className={styles.section}>
            <div className={styles.sectionHeader}>Risk Thresholds</div>
            {[
              { label: 'confidence_threshold',  value: confidence,  set: setConfidence },
              { label: 'min_risk_reward',        value: minRiskReward, set: setMinRiskReward },
              { label: 'max_position_pct',       value: maxPosPct,  set: setMaxPosPct },
              { label: 'max_open_positions',     value: maxOpen,    set: setMaxOpen },
              { label: 'daily_loss_limit_pct',   value: dailyLoss,  set: setDailyLoss },
              { label: 'max_drawdown_pct',       value: maxDrawdown, set: setMaxDrawdown, danger: true },
              { label: 'cooldown_after_loss_min', value: cooldown,   set: setCooldown },
              { label: 'signal_expiry_min',      value: signalExpiry, set: setSignalExpiry },
            ].map(f => (
              <div key={f.label} className={styles.fieldRow}>
                <span className={cls(styles.fieldLabel, f.danger && styles.fieldLabelDanger)}>
                  {f.label}
                </span>
                <input
                  className={cls(styles.fieldInput, f.danger && styles.fieldInputDanger)}
                  type="number"
                  value={f.value}
                  onChange={e => f.set(e.target.value)}
                />
              </div>
            ))}
          </div>

          {/* Strategies */}
          <div className={styles.section}>
            <div className={styles.sectionHeader}>Strategies</div>
            {STRATEGIES.map(s => (
              <div key={s.name} className={styles.strategyRow}>
                <div className={styles.strategyHeader} onClick={() => setExpandedStrategy(expandedStrategy === s.name ? null : s.name)}>
                  <span className={styles.strategyName}>{s.name}</span>
                  <Toggle
                    on={!!strategyOn[s.name]}
                    onToggle={() => setStrategyOn(prev => ({ ...prev, [s.name]: !prev[s.name] }))}
                  />
                  <span className={cls(styles.strategyChevron, expandedStrategy === s.name && styles.strategyChevronOpen)}>
                    ▶
                  </span>
                </div>
                {expandedStrategy === s.name && (
                  <div className={styles.strategyParams}>
                    {s.params.map(p => (
                      <div key={p.label} className={styles.fieldRow}>
                        <span className={styles.fieldLabel}>{p.label}</span>
                        <input className={styles.fieldInput} type="text" defaultValue={p.value} readOnly />
                      </div>
                    ))}
                  </div>
                )}
              </div>
            ))}
          </div>
        </div>

        {/* ════════════ RIGHT COLUMN ════════════ */}
        <div>
          {/* Model Roles */}
          <div className={styles.section}>
            <div className={styles.sectionHeader}>Model Roles</div>
            {MODEL_ROLES.map(m => (
              <div key={m.role} className={styles.modelRow}>
                <span className={styles.modelRole}>{m.role}</span>
                <span className={styles.modelName}>{m.model}</span>
                <button className={styles.swapBtn} type="button">SWAP</button>
              </div>
            ))}
          </div>

          {/* Cost Profile */}
          <div className={styles.section}>
            <div className={styles.sectionHeader}>Cost Profile</div>
            <div className={styles.toggleGroup}>
              {['BUDGET', 'BALANCED', 'PRO'].map(c => (
                <button key={c} className={segmentStyle(c, costProfile)} onClick={() => setCostProfile(c)}>
                  {c}
                </button>
              ))}
            </div>
            <div className={styles.fieldRow}>
              <span className={styles.fieldLabel}>Daily token budget</span>
              <input className={styles.fieldInput} type="number" value={dailyTokens} onChange={e => setDailyTokens(e.target.value)} />
            </div>
            <div className={styles.fieldRow}>
              <span className={styles.fieldLabel}>Weekly token budget</span>
              <input className={styles.fieldInput} type="number" value={weeklyTokens} onChange={e => setWeeklyTokens(e.target.value)} />
            </div>
            <div className={styles.fieldRow}>
              <span className={styles.fieldLabel}>Auto-degrade</span>
              <Toggle on={autoDegrade} onToggle={() => setAutoDegrade(!autoDegrade)} />
            </div>
          </div>

          {/* Notifications */}
          <div className={styles.section}>
            <div className={styles.sectionHeader}>Notifications</div>
            <div className={styles.fieldRow}>
              <span className={styles.fieldLabel}>Telegram</span>
              <Toggle on={telegram} onToggle={() => setTelegram(!telegram)} />
            </div>
            <div style={{ padding: 'var(--space-2) 0' }}>
              {Object.keys(notifyChecks).map(k => (
                <Checkbox
                  key={k}
                  on={notifyChecks[k]}
                  onToggle={() => setNotifyChecks(prev => ({ ...prev, [k]: !prev[k] }))}
                  label={k}
                />
              ))}
            </div>
            <div className={styles.fieldRow}>
              <span className={styles.fieldLabel}>Batch enabled</span>
              <Toggle on={batchEnabled} onToggle={() => setBatchEnabled(!batchEnabled)} />
            </div>
            <div className={styles.fieldRow}>
              <span className={styles.fieldLabel}>Batch window (min)</span>
              <input className={styles.fieldInput} type="number" value={batchWindow} onChange={e => setBatchWindow(e.target.value)} />
            </div>
            <div className={styles.fieldRow}>
              <span className={styles.fieldLabel}>Min confidence for alert</span>
              <input className={styles.fieldInput} type="number" value={minConfAlert} onChange={e => setMinConfAlert(e.target.value)} />
            </div>
            <div className={styles.fieldRow}>
              <span className={styles.fieldLabel}>Include HOLDs</span>
              <Toggle on={includeHolds} onToggle={() => setIncludeHolds(!includeHolds)} />
            </div>
          </div>

          {/* System */}
          <div className={styles.section}>
            <div className={styles.sectionHeader}>System</div>
            <button className={cls(styles.actionBtn, styles.actionBtnAmber)} type="button">
              RESTART ENGINE
            </button>
            <button className={cls(styles.actionBtn, styles.actionBtnRed)} type="button">
              RESET TO DEFAULTS
            </button>
            <div className={styles.versionString}>v2.4.1-alpha</div>
          </div>
        </div>
      </div>

      {/* ════════════ FOOTER ════════════ */}
      <div className={styles.footer}>
        <button className={styles.saveBtn} type="button">SAVE CHANGES</button>
        <button className={styles.discardBtn} type="button">DISCARD</button>
      </div>
    </div>
  );
}
