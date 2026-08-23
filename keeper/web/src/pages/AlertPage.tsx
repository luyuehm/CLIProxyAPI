import { useCallback, useEffect, useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { ApiError, appPath, fetchAlertChannels, createAlertChannel, updateAlertChannel, deleteAlertChannel, fetchAlertRules, createAlertRule, updateAlertRule, deleteAlertRule, fetchAlertEvents, retryAlertEvent } from '@/lib/api';
import type { AlertChannel, AlertChannelCreateRequest, AlertChannelUpdateRequest, AlertRule, AlertRuleCreateRequest, AlertRuleUpdateRequest, AlertEvent, AlertPlatform, AlertMetricType, AlertConditionOperator } from '@/lib/types';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { Modal } from '@/components/ui/Modal';
import { Select } from '@/components/ui/Select';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import styles from './AlertPage.module.scss';

type Tab = 'channels' | 'rules' | 'events';

const PLATFORM_OPTIONS: ReadonlyArray<{ value: AlertPlatform; labelKey: string }> = [
  { value: 'feishu', labelKey: 'alerts.platform_feishu' },
  { value: 'dingtalk', labelKey: 'alerts.platform_dingtalk' },
  { value: 'wecom', labelKey: 'alerts.platform_wecom' },
];

const METRIC_OPTIONS: ReadonlyArray<{ value: AlertMetricType; labelKey: string }> = [
  { value: 'usage_threshold', labelKey: 'alerts.metric_usage_threshold' },
  { value: 'quota_exhausted', labelKey: 'alerts.metric_quota_exhausted' },
  { value: 'error_rate', labelKey: 'alerts.metric_error_rate' },
];

const OP_OPTIONS: ReadonlyArray<{ value: AlertConditionOperator; labelKey: string }> = [
  { value: 'gt', labelKey: 'alerts.op_gt' },
  { value: 'gte', labelKey: 'alerts.op_gte' },
  { value: 'lt', labelKey: 'alerts.op_lt' },
  { value: 'lte', labelKey: 'alerts.op_lte' },
];

const PLATFORM_CLASS: Record<AlertPlatform, string> = {
  feishu: `${styles.platformBadge} ${styles.platformFeishu}`,
  dingtalk: `${styles.platformBadge} ${styles.platformDingtalk}`,
  wecom: `${styles.platformBadge} ${styles.platformWecom}`,
};

export function AlertPage({ onAuthRequired }: { onAuthRequired?: () => void }) {
  const { t } = useTranslation();
  const [tab, setTab] = useState<Tab>('channels');
  const [channels, setChannels] = useState<AlertChannel[]>([]);
  const [rules, setRules] = useState<AlertRule[]>([]);
  const [events, setEvents] = useState<AlertEvent[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState<{ kind: 'success' | 'error'; message: string } | null>(null);
  const [showChannelModal, setShowChannelModal] = useState(false);
  const [editingChannel, setEditingChannel] = useState<AlertChannel | null>(null);
  const [showRuleModal, setShowRuleModal] = useState(false);
  const [editingRule, setEditingRule] = useState<AlertRule | null>(null);

  const loadChannels = useCallback(async (signal?: AbortSignal) => {
    try {
      const rows = await fetchAlertChannels(signal);
      setChannels(rows);
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) { onAuthRequired?.(); return; }
      if (err instanceof DOMException && err.name === 'AbortError') return;
      setError(err instanceof Error ? err.message : 'Failed to load channels');
    }
  }, [onAuthRequired]);

  const loadRules = useCallback(async (signal?: AbortSignal) => {
    try {
      const rows = await fetchAlertRules(signal);
      setRules(rows);
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) { onAuthRequired?.(); return; }
      if (err instanceof DOMException && err.name === 'AbortError') return;
      setError(err instanceof Error ? err.message : 'Failed to load rules');
    }
  }, [onAuthRequired]);

  const loadEvents = useCallback(async (signal?: AbortSignal) => {
    try {
      const rows = await fetchAlertEvents(20, signal);
      setEvents(rows);
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) { onAuthRequired?.(); return; }
      if (err instanceof DOMException && err.name === 'AbortError') return;
      setError(err instanceof Error ? err.message : 'Failed to load events');
    }
  }, [onAuthRequired]);

  useEffect(() => {
    setLoading(true);
    setError('');
    const ac = new AbortController();
    const load = async () => {
      try {
        if (tab === 'channels') await loadChannels(ac.signal);
        else if (tab === 'rules') await loadRules(ac.signal);
        else await loadEvents(ac.signal);
      } finally {
        if (!ac.signal.aborted) setLoading(false);
      }
    };
    void load();
    return () => ac.abort();
  }, [tab, loadChannels, loadRules, loadEvents]);

  const handleDeleteChannel = useCallback(async (channel: AlertChannel) => {
    if (!window.confirm(t('alerts.delete_channel_confirm', { name: channel.name }))) return;
    try {
      await deleteAlertChannel(channel.id);
      setNotice({ kind: 'success', message: t('alerts.channel_deleted') });
      void loadChannels();
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) { onAuthRequired?.(); return; }
      setNotice({ kind: 'error', message: err instanceof Error ? err.message : t('alerts.delete_failed') });
    }
  }, [loadChannels, onAuthRequired, t]);

  const handleDeleteRule = useCallback(async (rule: AlertRule) => {
    if (!window.confirm(t('alerts.delete_rule_confirm', { name: rule.name }))) return;
    try {
      await deleteAlertRule(rule.id);
      setNotice({ kind: 'success', message: t('alerts.rule_deleted') });
      void loadRules();
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) { onAuthRequired?.(); return; }
      setNotice({ kind: 'error', message: err instanceof Error ? err.message : t('alerts.delete_failed') });
    }
  }, [loadRules, onAuthRequired, t]);

  const handleRetryEvent = useCallback(async (event: AlertEvent) => {
    try {
      const result = await retryAlertEvent(event.id);
      if (result.retry_error) {
        setNotice({ kind: 'error', message: `Retry: ${result.retry_error}` });
      } else {
        setNotice({ kind: 'success', message: t('alerts.event_retried') });
      }
      void loadEvents();
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) { onAuthRequired?.(); return; }
      setNotice({ kind: 'error', message: err instanceof Error ? err.message : t('alerts.retry_failed') });
    }
  }, [loadEvents, onAuthRequired, t]);

  useEffect(() => {
    if (!notice) return;
    const timer = window.setTimeout(() => setNotice(null), 4000);
    return () => window.clearTimeout(timer);
  }, [notice]);

  const channelName = (id: number) => channels.find(c => c.id === id)?.name ?? `#${id}`;

  return (
    <div className={styles.pageShell}>
      <div className={styles.pageFrame}>
        <div className={styles.header}>
          <div>
            <h2 className={styles.title}>{t('alerts.title')}</h2>
            <p className={styles.subtitle}>{t('alerts.subtitle')}</p>
          </div>
          <div className={styles.headerActions}>
            <Button variant="ghost" onClick={() => { window.location.href = appPath('/'); }}>
              {t('alerts.back_to_dashboard')}
            </Button>
            {tab !== 'events' && (
              <Button onClick={() => {
                if (tab === 'channels') setShowChannelModal(true);
                else setShowRuleModal(true);
              }}>
                {tab === 'channels' ? t('alerts.new_channel') : t('alerts.new_rule')}
              </Button>
            )}
          </div>
        </div>

        <div className={styles.tabs}>
          <button className={`${styles.tab} ${tab === 'channels' ? styles.tabActive : ''}`} onClick={() => setTab('channels')}>{t('alerts.tab_channels')}</button>
          <button className={`${styles.tab} ${tab === 'rules' ? styles.tabActive : ''}`} onClick={() => setTab('rules')}>{t('alerts.tab_rules')}</button>
          <button className={`${styles.tab} ${tab === 'events' ? styles.tabActive : ''}`} onClick={() => setTab('events')}>{t('alerts.tab_events')}</button>
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
            <span>{t('users.loading')}</span>
          </div>
        ) : tab === 'channels' ? (
          <ChannelsTable channels={channels} t={t} onEdit={setEditingChannel} onDelete={handleDeleteChannel} />
        ) : tab === 'rules' ? (
          <RulesTable rules={rules} channelName={channelName} t={t} onEdit={setEditingRule} onDelete={handleDeleteRule} />
        ) : (
          <EventsTable events={events} channelName={channelName} t={t} onRetry={handleRetryEvent} />
        )}
      </div>

      {showChannelModal && (
        <ChannelFormModal
          onClose={() => setShowChannelModal(false)}
          onSaved={() => {
            setShowChannelModal(false);
            setNotice({ kind: 'success', message: t('alerts.channel_saved') });
            void loadChannels();
          }}
          onAuthRequired={onAuthRequired}
        />
      )}
      {editingChannel && (
        <ChannelFormModal
          channel={editingChannel}
          onClose={() => setEditingChannel(null)}
          onSaved={() => {
            setEditingChannel(null);
            setNotice({ kind: 'success', message: t('alerts.channel_saved') });
            void loadChannels();
          }}
          onAuthRequired={onAuthRequired}
        />
      )}
      {showRuleModal && (
        <RuleFormModal
          channels={channels}
          onClose={() => setShowRuleModal(false)}
          onSaved={() => {
            setShowRuleModal(false);
            setNotice({ kind: 'success', message: t('alerts.rule_saved') });
            void loadRules();
          }}
          onAuthRequired={onAuthRequired}
        />
      )}
      {editingRule && (
        <RuleFormModal
          channels={channels}
          rule={editingRule}
          onClose={() => setEditingRule(null)}
          onSaved={() => {
            setEditingRule(null);
            setNotice({ kind: 'success', message: t('alerts.rule_saved') });
            void loadRules();
          }}
          onAuthRequired={onAuthRequired}
        />
      )}
    </div>
  );
}

function ChannelsTable({ channels, t, onEdit, onDelete }: {
  channels: AlertChannel[];
  t: (key: string) => string;
  onEdit: (c: AlertChannel) => void;
  onDelete: (c: AlertChannel) => void;
}) {
  return (
    <div className={styles.tableContainer}>
      <table className={styles.table}>
        <thead>
          <tr>
            <th>{t('alerts.channel_name')}</th>
            <th>{t('alerts.platform')}</th>
            <th>{t('alerts.webhook_url')}</th>
            <th>{t('alerts.status')}</th>
            <th>{t('alerts.actions')}</th>
          </tr>
        </thead>
        <tbody>
          {channels.length === 0 && (
            <tr><td colSpan={5} className={styles.emptyCell}>{t('alerts.no_channels')}</td></tr>
          )}
          {channels.map(ch => (
            <tr key={ch.id}>
              <td><strong>{ch.name}</strong></td>
              <td><span className={PLATFORM_CLASS[ch.platform]}>{t(`alerts.platform_${ch.platform}`)}</span></td>
              <td><code style={{ fontSize: '0.8125rem' }}>{ch.webhook_url}</code></td>
              <td><span className={ch.enabled ? styles.statusEnabled : styles.statusDisabled}>{ch.enabled ? t('alerts.enabled') : t('alerts.disabled')}</span></td>
              <td>
                <div className={styles.actions}>
                  <Button variant="ghost" size="sm" onClick={() => onEdit(ch)}>{t('common.edit')}</Button>
                  <Button variant="danger" size="sm" onClick={() => void onDelete(ch)}>{t('common.delete')}</Button>
                </div>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function RulesTable({ rules, channelName, t, onEdit, onDelete }: {
  rules: AlertRule[];
  channelName: (id: number) => string;
  t: (key: string) => string;
  onEdit: (r: AlertRule) => void;
  onDelete: (r: AlertRule) => void;
}) {
  return (
    <div className={styles.tableContainer}>
      <table className={styles.table}>
        <thead>
          <tr>
            <th>{t('alerts.rule_name')}</th>
            <th>{t('alerts.metric_type')}</th>
            <th>{t('alerts.condition')}</th>
            <th>{t('alerts.channel')}</th>
            <th>{t('alerts.status')}</th>
            <th>{t('alerts.actions')}</th>
          </tr>
        </thead>
        <tbody>
          {rules.length === 0 && (
            <tr><td colSpan={6} className={styles.emptyCell}>{t('alerts.no_rules')}</td></tr>
          )}
          {rules.map(r => (
            <tr key={r.id}>
              <td><strong>{r.name}</strong></td>
              <td>{t(`alerts.metric_${r.metric_type}`)}</td>
              <td><code>{r.condition_op} {r.condition_val}</code></td>
              <td>{channelName(r.channel_id)}</td>
              <td><span className={r.enabled ? styles.statusEnabled : styles.statusDisabled}>{r.enabled ? t('alerts.enabled') : t('alerts.disabled')}</span></td>
              <td>
                <div className={styles.actions}>
                  <Button variant="ghost" size="sm" onClick={() => onEdit(r)}>{t('common.edit')}</Button>
                  <Button variant="danger" size="sm" onClick={() => void onDelete(r)}>{t('common.delete')}</Button>
                </div>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function EventsTable({ events, channelName, t, onRetry }: {
  events: AlertEvent[];
  channelName: (id: number) => string;
  t: (key: string) => string;
  onRetry: (e: AlertEvent) => void;
}) {
  const statusClass = (status: string) => {
    switch (status) {
      case 'pending': return styles.statusPending;
      case 'sent': return styles.statusSent;
      case 'failed': return styles.statusFailed;
      default: return '';
    }
  };
  return (
    <div className={styles.tableContainer}>
      <table className={styles.table}>
        <thead>
          <tr>
            <th>{t('alerts.event_id')}</th>
            <th>{t('alerts.channel')}</th>
            <th>{t('alerts.status')}</th>
            <th>{t('alerts.message')}</th>
            <th>{t('alerts.attempts')}</th>
            <th>{t('alerts.last_error')}</th>
            <th>{t('alerts.actions')}</th>
          </tr>
        </thead>
        <tbody>
          {events.length === 0 && (
            <tr><td colSpan={7} className={styles.emptyCell}>{t('alerts.no_events')}</td></tr>
          )}
          {events.map(e => (
            <tr key={e.id}>
              <td>#{e.id}</td>
              <td>{channelName(e.channel_id)}</td>
              <td><span className={`${styles.statusBadge} ${statusClass(e.status)}`}>{e.status}</span></td>
              <td><div className={styles.eventMessage} title={e.message}>{e.message}</div></td>
              <td>{e.attempt_count}</td>
              <td>{e.last_error ? <div className={styles.eventError} title={e.last_error}>{e.last_error}</div> : '—'}</td>
              <td>
                {e.status === 'failed' && (
                  <Button variant="ghost" size="sm" className={styles.retryBtn} onClick={() => void onRetry(e)}>
                    {t('alerts.retry')}
                  </Button>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function ChannelFormModal({ channel, onClose, onSaved, onAuthRequired }: {
  channel?: AlertChannel;
  onClose: () => void;
  onSaved: () => void;
  onAuthRequired?: () => void;
}) {
  const { t } = useTranslation();
  const [name, setName] = useState(channel?.name ?? '');
  const [platform, setPlatform] = useState<AlertPlatform>(channel?.platform ?? 'feishu');
  const [webhookUrl, setWebhookUrl] = useState(channel?.webhook_url ?? '');
  const [secret, setSecret] = useState(channel?.secret ?? '');
  const [enabled, setEnabled] = useState(channel?.enabled ?? true);
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState('');

  const handleSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setSaving(true);
    setErr('');
    try {
      if (channel) {
        const payload: AlertChannelUpdateRequest = {};
        if (name !== channel.name) payload.name = name;
        if (platform !== channel.platform) payload.platform = platform;
        if (webhookUrl !== channel.webhook_url) payload.webhook_url = webhookUrl;
        if (secret !== (channel.secret ?? '')) payload.secret = secret || undefined;
        if (enabled !== channel.enabled) payload.enabled = enabled;
        await updateAlertChannel(channel.id, payload);
      } else {
        await createAlertChannel({ name, platform, webhook_url: webhookUrl, secret: secret || undefined, enabled });
      }
      onSaved();
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) { onAuthRequired?.(); return; }
      setErr(error instanceof Error ? error.message : t('alerts.save_failed'));
    } finally {
      setSaving(false);
    }
  };

  const platformOpts = PLATFORM_OPTIONS.map(({ value, labelKey }) => ({ value, label: t(labelKey) }));

  return (
    <Modal
      open
      title={channel ? t('alerts.edit_channel') : t('alerts.new_channel')}
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={saving}>{t('common.cancel')}</Button>
          <Button type="submit" form="channel-form" loading={saving}>{saving ? t('users.saving') : t('common.save')}</Button>
        </>
      }
    >
      <form id="channel-form" className={styles.form} onSubmit={event => void handleSubmit(event)}>
        {err && <div className={styles.errorBox}>{err}</div>}
        <Input
          label={t('alerts.channel_name')}
          value={name}
          onChange={e => setName(e.target.value)}
          required
        />
        <div className={styles.formField}>
          <span className={styles.formLabel}>{t('alerts.platform')}</span>
          <Select
            value={platform}
            options={platformOpts}
            onChange={v => setPlatform(v as AlertPlatform)}
            ariaLabel={t('alerts.platform')}
          />
        </div>
        <Input
          label={t('alerts.webhook_url')}
          value={webhookUrl}
          onChange={e => setWebhookUrl(e.target.value)}
          required
        />
        <Input
          label={t('alerts.secret')}
          value={secret}
          onChange={e => setSecret(e.target.value)}
          hint={t('alerts.secret_hint')}
        />
        <label className={styles.formField} style={{ flexDirection: 'row', alignItems: 'center', gap: '0.5rem' }}>
          <input type="checkbox" checked={enabled} onChange={e => setEnabled(e.target.checked)} />
          <span className={styles.formLabel}>{t('alerts.enabled')}</span>
        </label>
      </form>
    </Modal>
  );
}

function RuleFormModal({ channels, rule, onClose, onSaved, onAuthRequired }: {
  channels: AlertChannel[];
  rule?: AlertRule;
  onClose: () => void;
  onSaved: () => void;
  onAuthRequired?: () => void;
}) {
  const { t } = useTranslation();
  const [name, setName] = useState(rule?.name ?? '');
  const [metricType, setMetricType] = useState<AlertMetricType>(rule?.metric_type ?? 'usage_threshold');
  const [conditionOp, setConditionOp] = useState<AlertConditionOperator>(rule?.condition_op ?? 'gt');
  const [conditionVal, setConditionVal] = useState(String(rule?.condition_val ?? '0'));
  const [channelId, setChannelId] = useState(String(rule?.channel_id ?? (channels[0]?.id ?? '')));
  const [enabled, setEnabled] = useState(rule?.enabled ?? true);
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState('');

  const handleSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setSaving(true);
    setErr('');
    try {
      if (rule) {
        const payload: AlertRuleUpdateRequest = {};
        if (name !== rule.name) payload.name = name;
        if (metricType !== rule.metric_type) payload.metric_type = metricType;
        if (conditionOp !== rule.condition_op) payload.condition_op = conditionOp;
        const val = Number(conditionVal);
        if (val !== rule.condition_val) payload.condition_val = val;
        const cid = Number(channelId);
        if (cid !== rule.channel_id) payload.channel_id = cid;
        if (enabled !== rule.enabled) payload.enabled = enabled;
        await updateAlertRule(rule.id, payload);
      } else {
        await createAlertRule({
          name,
          metric_type: metricType,
          condition_op: conditionOp,
          condition_val: Number(conditionVal),
          channel_id: Number(channelId),
          enabled,
        });
      }
      onSaved();
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) { onAuthRequired?.(); return; }
      setErr(error instanceof Error ? error.message : t('alerts.save_failed'));
    } finally {
      setSaving(false);
    }
  };

  const metricOpts = METRIC_OPTIONS.map(({ value, labelKey }) => ({ value, label: t(labelKey) }));
  const opOpts = OP_OPTIONS.map(({ value, labelKey }) => ({ value, label: t(labelKey) }));
  const channelOpts = channels.map(ch => ({ value: String(ch.id), label: ch.name }));

  return (
    <Modal
      open
      title={rule ? t('alerts.edit_rule') : t('alerts.new_rule')}
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={saving}>{t('common.cancel')}</Button>
          <Button type="submit" form="rule-form" loading={saving}>{saving ? t('users.saving') : t('common.save')}</Button>
        </>
      }
    >
      <form id="rule-form" className={styles.form} onSubmit={event => void handleSubmit(event)}>
        {err && <div className={styles.errorBox}>{err}</div>}
        <Input
          label={t('alerts.rule_name')}
          value={name}
          onChange={e => setName(e.target.value)}
          required
        />
        <div className={styles.formField}>
          <span className={styles.formLabel}>{t('alerts.metric_type')}</span>
          <Select
            value={metricType}
            options={metricOpts}
            onChange={v => setMetricType(v as AlertMetricType)}
            ariaLabel={t('alerts.metric_type')}
          />
        </div>
        <div className={styles.formField} style={{ flexDirection: 'row', gap: '0.5rem', alignItems: 'flex-end' }}>
          <div style={{ flex: 1 }}>
            <Select
              value={conditionOp}
              options={opOpts}
              onChange={v => setConditionOp(v as AlertConditionOperator)}
              ariaLabel={t('alerts.condition_op')}
            />
          </div>
          <div style={{ flex: 2 }}>
            <Input
              label={t('alerts.condition_val')}
              type="number"
              value={conditionVal}
              onChange={e => setConditionVal(e.target.value)}
              required
            />
          </div>
        </div>
        <div className={styles.formField}>
          <span className={styles.formLabel}>{t('alerts.channel')}</span>
          {channelOpts.length > 0 ? (
            <Select
              value={channelId}
              options={channelOpts}
              onChange={v => setChannelId(v)}
              ariaLabel={t('alerts.channel')}
            />
          ) : (
            <span className={styles.formLabel} style={{ color: '#cf1322' }}>{t('alerts.no_channels_hint')}</span>
          )}
        </div>
        <label className={styles.formField} style={{ flexDirection: 'row', alignItems: 'center', gap: '0.5rem' }}>
          <input type="checkbox" checked={enabled} onChange={e => setEnabled(e.target.checked)} />
          <span className={styles.formLabel}>{t('alerts.enabled')}</span>
        </label>
      </form>
    </Modal>
  );
}