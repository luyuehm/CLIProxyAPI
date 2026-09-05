import { useCallback, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { QRCodeSVG } from 'qrcode.react';
import { Card } from '@/components/ui/Card';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import type { MFASetupResponse } from '@/lib/types';
import { enableMFA, fetchMFASetup } from '@/lib/api';
import styles from '@/pages/UsagePage.module.scss';

export interface MFASettingsCardProps {
  onNotice?: (kind: 'success' | 'error', message: string) => void;
  onAuthRequired?: () => void;
}

function isApiStatus(error: unknown, status: number): boolean {
  return (
    error instanceof Error
    && 'status' in error
    && typeof (error as { status?: unknown }).status === 'number'
    && (error as { status: number }).status === status
  );
}

export function MFASettingsCard({ onNotice, onAuthRequired }: MFASettingsCardProps) {
  const { t } = useTranslation();
  const [loading, setLoading] = useState(false);
  const [setup, setSetup] = useState<MFASetupResponse | null>(null);
  const [code, setCode] = useState('');
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');

  const handleStartSetup = useCallback(async () => {
    setLoading(true);
    setError('');
    try {
      const response = await fetchMFASetup();
      setSetup(response);
      setCode('');
    } catch (err) {
      if (isApiStatus(err, 401)) {
        onAuthRequired?.();
        return;
      }
      setError(t('usage_stats.mfa_setup_failed'));
    } finally {
      setLoading(false);
    }
  }, [onAuthRequired, t]);

  const handleConfirm = useCallback(async () => {
    const normalized = code.trim();
    if (!normalized || !setup) return;
    setSaving(true);
    setError('');
    try {
      await enableMFA(setup.secret, normalized);
      setSetup(null);
      setCode('');
      onNotice?.('success', t('usage_stats.mfa_enabled'));
    } catch (err) {
      if (isApiStatus(err, 401)) {
        setError(t('usage_stats.mfa_invalid_code'));
      } else {
        setError(t('usage_stats.mfa_setup_failed'));
      }
    } finally {
      setSaving(false);
    }
  }, [code, onNotice, setup, t]);

  const canStart = !loading && !saving && !setup;
  const canConfirm = Boolean(setup) && code.trim().length === 6 && !saving;

  return (
    <Card
      title={t('usage_stats.mfa_settings_title')}
      subtitle={t('usage_stats.mfa_settings_subtitle')}
      className={`${styles.detailsFixedCard} ${styles.mfaSettingsCard}`}
    >
      <div className={styles.mfaSettingsBody}>
        {!setup ? (
          <>
            <p className={styles.mfaSettingsIntro}>{t('usage_stats.mfa_settings_intro')}</p>
            <Button type="button" appearance="action" onClick={() => void handleStartSetup()} loading={loading} disabled={!canStart}>
              {t('usage_stats.mfa_setup_start')}
            </Button>
          </>
        ) : (
          <div className={styles.mfaSetupFlow}>
            <QRCodeSVG value={setup.otp_url} size={176} className={styles.mfaQrCode} />
            <div className={styles.mfaSecretRow}>
              <code className={styles.mfaSecret}>{setup.secret}</code>
            </div>
            <p className={styles.mfaSetupHint}>{t('usage_stats.mfa_setup_hint')}</p>
            <Input
              type="text"
              inputMode="numeric"
              autoComplete="one-time-code"
              label={t('auth.mfa_code_label')}
              placeholder={t('auth.mfa_code_placeholder')}
              value={code}
              onChange={(event) => setCode(event.target.value.replace(/\D/g, '').slice(0, 6))}
              error={error || undefined}
              disabled={saving}
            />
            <div className={styles.mfaSetupActions}>
              <Button
                type="button"
                variant="secondary"
                appearance="action"
                onClick={() => setSetup(null)}
                disabled={saving}
              >
                {t('common.cancel')}
              </Button>
              <Button type="button" appearance="action" loading={saving} disabled={!canConfirm} onClick={() => void handleConfirm()}>
                {t('usage_stats.mfa_enable_confirm')}
              </Button>
            </div>
          </div>
        )}
      </div>
    </Card>
  );
}

export default MFASettingsCard;
