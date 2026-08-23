import { useCallback, useEffect, useMemo, useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { ApiError, appPath, fetchBudget, fetchBudgetUsage, fetchBudgetReport, updateBudget } from '@/lib/api';
import type { BudgetConfig, BudgetReport, BudgetUsage, BudgetUpdateRequest } from '@/lib/types';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { Select } from '@/components/ui/Select';
import { Card } from '@/components/ui/Card';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { Modal } from '@/components/ui/Modal';
import styles from './BudgetPage.module.scss';

const PERIOD_OPTIONS = [
  { value: 'monthly', labelKey: 'budget.period_monthly' },
  { value: 'quarterly', labelKey: 'budget.period_quarterly' },
  { value: 'yearly', labelKey: 'budget.period_yearly' },
];

export function BudgetPage({ onAuthRequired }: { onAuthRequired?: () => void }) {
  const { t } = useTranslation();

  const [selectedPeriod, setSelectedPeriod] = useState('monthly');
  const [config, setConfig] = useState<BudgetConfig | null>(null);
  const [usage, setUsage] = useState<BudgetUsage | null>(null);
  const [report, setReport] = useState<BudgetReport | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState<{ kind: 'success' | 'error'; message: string } | null>(null);
  const [showSettings, setShowSettings] = useState(false);

  const loadData = useCallback(async (period: string) => {
    setLoading(true);
    setError('');
    try {
      const [cfg, usg, rpt] = await Promise.all([
        fetchBudget(period),
        fetchBudgetUsage(period),
        fetchBudgetReport(period),
      ]);
      setConfig(cfg);
      setUsage(usg);
      setReport(rpt);
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        onAuthRequired?.();
        return;
      }
      setError(err instanceof Error ? err.message : t('budget.load_failed'));
    } finally {
      setLoading(false);
    }
  }, [onAuthRequired, t]);

  useEffect(() => {
    void loadData(selectedPeriod);
  }, [loadData, selectedPeriod]);

  useEffect(() => {
    if (!notice) return;
    const timer = window.setTimeout(() => setNotice(null), 4000);
    return () => window.clearTimeout(timer);
  }, [notice]);

  const periodOptions = PERIOD_OPTIONS.map(({ value, labelKey }) => ({
    value,
    label: t(labelKey),
  }));

  const handlePeriodChange = (period: string) => {
    setSelectedPeriod(period);
  };

  const handleSettingsSaved = () => {
    setShowSettings(false);
    setNotice({ kind: 'success', message: t('budget.saved') });
    void loadData(selectedPeriod);
  };

  return (
    <div className={styles.pageShell}>
      <div className={styles.pageFrame}>
        <div className={styles.header}>
          <div>
            <h2 className={styles.title}>{t('budget.title')}</h2>
            <p className={styles.subtitle}>{t('budget.subtitle')}</p>
          </div>
          <div className={styles.headerActions}>
            <Button variant="ghost" onClick={() => { window.location.href = appPath('/'); }}>
              {t('budget.back_to_dashboard')}
            </Button>
            <Button onClick={() => setShowSettings(true)}>
              {t('budget.settings')}
            </Button>
          </div>
        </div>

        <div className={styles.periodBar}>
          <span className={styles.periodLabel}>{t('budget.period_label')}</span>
          <Select
            value={selectedPeriod}
            options={periodOptions}
            onChange={handlePeriodChange}
            ariaLabel={t('budget.period_label')}
          />
        </div>

        {notice && (
          <div className={`${styles.notice} ${notice.kind === 'success' ? styles.noticeSuccess : styles.noticeError}`} role="status">
            {notice.message}
          </div>
        )}
        {error && <div className={styles.errorBox}>{error}</div>}

        {loading ? (
          <div className={styles.loadingRow}>
            <LoadingSpinner size={20} />
            <span>{t('common.loading')}</span>
          </div>
        ) : (
          <div className={styles.content}>
            {usage && (
              <Card
                title={t('budget.usage_card_title')}
                className={styles.card}
              >
                <div className={styles.usageSection}>
                  <div className={styles.usageSummary}>
                    <div className={styles.usageStat}>
                      <span className={styles.usageStatLabel}>{t('budget.amount')}</span>
                      <span className={styles.usageStatValue}>{formatUSD(usage.amount)}</span>
                    </div>
                    <div className={styles.usageStat}>
                      <span className={styles.usageStatLabel}>{t('budget.spent')}</span>
                      <span className={`${styles.usageStatValue} ${usage.exceeded ? styles.overBudget : ''}`}>
                        {formatUSD(usage.spent)}
                      </span>
                    </div>
                    <div className={styles.usageStat}>
                      <span className={styles.usageStatLabel}>{t('budget.remaining')}</span>
                      <span className={styles.usageStatValue}>{formatUSD(usage.remaining)}</span>
                    </div>
                  </div>

                  <div className={styles.progressBarContainer}>
                    <div
                      className={`${styles.progressBar} ${usage.exceeded ? styles.progressExceeded : usage.usage_percent >= usage.alert_threshold ? styles.progressWarning : ''}`}
                      style={{ width: `${Math.min(usage.usage_percent, 100)}%` }}
                    />
                    <span className={styles.progressLabel}>
                      {usage.usage_percent.toFixed(1)}%
                    </span>
                  </div>

                  {usage.alert_fired && (
                    <div className={styles.alertBanner} role="alert">
                      {usage.exceeded ? t('budget.alert_exceeded') : t('budget.alert_threshold_reached')}
                    </div>
                  )}
                  {!usage.cost_available && (
                    <div className={styles.hint}>{t('budget.cost_unavailable_hint')}</div>
                  )}
                </div>
              </Card>
            )}

            {report && report.items.length > 0 && (
              <Card
                title={t('budget.report_card_title')}
                className={styles.card}
              >
                <div className={styles.tableContainer}>
                  <table className={styles.table}>
                    <thead>
                      <tr>
                        <th>{t('budget.report_model')}</th>
                        <th className={styles.numericCol}>{t('budget.report_requests')}</th>
                        <th className={styles.numericCol}>{t('budget.report_tokens')}</th>
                        <th className={styles.numericCol}>{t('budget.report_cost')}</th>
                        <th className={styles.numericCol}>{t('budget.report_share')}</th>
                      </tr>
                    </thead>
                    <tbody>
                      {report.items.map((item) => (
                        <tr key={item.model}>
                          <td>{item.model}</td>
                          <td className={styles.numericCol}>{item.requests.toLocaleString()}</td>
                          <td className={styles.numericCol}>{item.total_tokens.toLocaleString()}</td>
                          <td className={styles.numericCol}>{formatUSD(item.cost)}</td>
                          <td className={styles.numericCol}>{item.cost_share.toFixed(1)}%</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              </Card>
            )}

            {report && report.items.length === 0 && (
              <Card className={styles.card}>
                <div className={styles.emptyState}>{t('budget.report_empty')}</div>
              </Card>
            )}
          </div>
        )}
      </div>

      {showSettings && config && (
        <BudgetSettingsModal
          config={config}
          onClose={() => setShowSettings(false)}
          onSaved={handleSettingsSaved}
          onAuthRequired={onAuthRequired}
        />
      )}
    </div>
  );
}

function formatUSD(value: number): string {
  return `$${value.toFixed(2)}`;
}

interface BudgetSettingsModalProps {
  config: BudgetConfig;
  onClose: () => void;
  onSaved: () => void;
  onAuthRequired?: () => void;
}

function BudgetSettingsModal({ config, onClose, onSaved, onAuthRequired }: BudgetSettingsModalProps) {
  const { t } = useTranslation();
  const [amount, setAmount] = useState(String(config.amount));
  const [alertThreshold, setAlertThreshold] = useState(String(config.alert_threshold));
  const [alertEnabled, setAlertEnabled] = useState(config.alert_enabled);
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState('');

  const periodOptions = useMemo(() => (
    PERIOD_OPTIONS.map(({ value, labelKey }) => ({ value, label: t(labelKey) }))
  ), [t]);

  const handleSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setSaving(true);
    setErr('');
    try {
      const threshold = Number(alertThreshold);
      if (threshold < 0 || threshold > 100) {
        setErr(t('budget.threshold_invalid'));
        setSaving(false);
        return;
      }
      const payload: BudgetUpdateRequest = {
        period: config.period,
        amount: Number(amount),
        alert_threshold: threshold,
        alert_enabled: alertEnabled,
      };
      await updateBudget(payload);
      onSaved();
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) {
        onAuthRequired?.();
        return;
      }
      setErr(error instanceof Error ? error.message : t('budget.save_failed'));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Modal
      open
      title={t('budget.settings_title')}
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={saving}>
            {t('common.cancel')}
          </Button>
          <Button type="submit" form="budget-form" loading={saving}>
            {saving ? t('common.loading') : t('common.save')}
          </Button>
        </>
      }
    >
      <form id="budget-form" className={styles.form} onSubmit={(event) => void handleSubmit(event)}>
        {err && <div className={styles.errorBox}>{err}</div>}
        <div className={styles.formField}>
          <span className={styles.formLabel}>{t('budget.period_label')}</span>
          <Select
            value={config.period}
            options={periodOptions}
            onChange={() => {}}
            ariaLabel={t('budget.period_label')}
            disabled
          />
        </div>
        <Input
          type="number"
          label={t('budget.amount_label')}
          value={amount}
          onChange={(event) => setAmount(event.target.value)}
          required
          min={0}
          step={0.01}
        />
        <Input
          type="number"
          label={t('budget.alert_threshold_label')}
          value={alertThreshold}
          onChange={(event) => setAlertThreshold(event.target.value)}
          hint={t('budget.alert_threshold_hint')}
          min={0}
          max={100}
          step={1}
        />
        <label className={styles.checkboxRow}>
          <input
            type="checkbox"
            checked={alertEnabled}
            onChange={(event) => setAlertEnabled(event.target.checked)}
          />
          <span>{t('budget.alert_enabled_label')}</span>
        </label>
      </form>
    </Modal>
  );
}
