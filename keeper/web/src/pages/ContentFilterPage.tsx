import { useCallback, useEffect, useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import {
  appPath,
  ApiError,
  fetchContentFilterRules,
  createContentFilterRule,
  updateContentFilterRule,
  deleteContentFilterRule,
  fetchContentFilterLogs,
  testContentFilter,
} from '@/lib/api';
import type {
  ContentFilterRule,
  ContentFilterLog,
  FilterAction,
  FilterScenario,
  FilterTextResult,
} from '@/lib/types';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { Modal } from '@/components/ui/Modal';
import { Select } from '@/components/ui/Select';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import styles from './ContentFilterPage.module.scss';

type ActiveTab = 'rules' | 'words' | 'logs' | 'playground';

const PII_TYPE_OPTIONS = [
  { key: 'phone', labelKey: 'content_filter.pii_phone' },
  { key: 'id_card', labelKey: 'content_filter.pii_id_card' },
  { key: 'email', labelKey: 'content_filter.pii_email' },
  { key: 'bank_card', labelKey: 'content_filter.pii_bank_card' },
  { key: 'medical_record', labelKey: 'content_filter.pii_medical_record' },
  { key: 'passport', labelKey: 'content_filter.pii_passport' },
];

const SCENARIO_OPTIONS: Array<{ value: FilterScenario; labelKey: string }> = [
  { value: 'general', labelKey: 'content_filter.scenario_general' },
  { value: 'finance', labelKey: 'content_filter.scenario_finance' },
  { value: 'medical', labelKey: 'content_filter.scenario_medical' },
  { value: 'custom', labelKey: 'content_filter.scenario_custom' },
];

const ACTION_OPTIONS: Array<{ value: FilterAction; labelKey: string }> = [
  { value: 'mask', labelKey: 'content_filter.action_mask' },
  { value: 'redact', labelKey: 'content_filter.action_redact' },
  { value: 'block', labelKey: 'content_filter.action_block' },
];

export function ContentFilterPage({ onAuthRequired }: { onAuthRequired?: () => void }) {
  const { t } = useTranslation();
  const [activeTab, setActiveTab] = useState<ActiveTab>('rules');

  // Rules state
  const [rules, setRules] = useState<ContentFilterRule[]>([]);
  const [rulesLoading, setRulesLoading] = useState(true);
  const [showRuleModal, setShowRuleModal] = useState(false);
  const [editingRule, setEditingRule] = useState<ContentFilterRule | null>(null);

  // Logs state
  const [logs, setLogs] = useState<ContentFilterLog[]>([]);
  const [logsTotal, setLogsTotal] = useState(0);
  const [logsLoading, setLogsLoading] = useState(false);
  const [logFilterType, setLogFilterType] = useState<string>('');
  const [logAction, setLogAction] = useState<string>('');

  // Playground state
  const [testText, setTestText] = useState(
    '您好，客户张三电话 13812345678，身份证 110101199003072345，银行卡 6222021234567890123，就诊卡号 MZ20230819001，请勿泄露支付密码。'
  );
  const [testModel, setTestModel] = useState('gpt-4o');
  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState<FilterTextResult | null>(null);

  // Feedback notice
  const [notice, setNotice] = useState<{ kind: 'success' | 'error'; message: string } | null>(null);

  const loadRules = useCallback(async () => {
    setRulesLoading(true);
    try {
      const data = await fetchContentFilterRules();
      setRules(data);
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        onAuthRequired?.();
        return;
      }
      setNotice({ kind: 'error', message: err instanceof Error ? err.message : 'Failed to load rules' });
    } finally {
      setRulesLoading(false);
    }
  }, [onAuthRequired]);

  const loadLogs = useCallback(async () => {
    setLogsLoading(true);
    try {
      const resp = await fetchContentFilterLogs({
        filter_type: logFilterType || undefined,
        action: logAction || undefined,
        limit: 50,
      });
      setLogs(resp.logs);
      setLogsTotal(resp.total);
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        onAuthRequired?.();
        return;
      }
      setNotice({ kind: 'error', message: err instanceof Error ? err.message : 'Failed to load logs' });
    } finally {
      setLogsLoading(false);
    }
  }, [logAction, logFilterType, onAuthRequired]);

  useEffect(() => {
    void loadRules();
  }, [loadRules]);

  useEffect(() => {
    if (activeTab === 'logs') {
      void loadLogs();
    }
  }, [activeTab, loadLogs]);

  useEffect(() => {
    if (!notice) return;
    const timer = window.setTimeout(() => setNotice(null), 4000);
    return () => window.clearTimeout(timer);
  }, [notice]);

  const handleToggleRule = async (rule: ContentFilterRule) => {
    try {
      const updated = await updateContentFilterRule(rule.id, { enabled: !rule.enabled });
      setRules((prev) => prev.map((r) => (r.id === updated.id ? updated : r)));
      setNotice({ kind: 'success', message: t('content_filter.rule_saved') });
    } catch (err) {
      setNotice({ kind: 'error', message: err instanceof Error ? err.message : t('content_filter.save_failed') });
    }
  };

  const handleDeleteRule = async (rule: ContentFilterRule) => {
    if (!window.confirm(t('content_filter.delete_confirm', { name: rule.name }))) return;
    try {
      await deleteContentFilterRule(rule.id);
      setNotice({ kind: 'success', message: t('content_filter.rule_deleted') });
      void loadRules();
    } catch (err) {
      setNotice({ kind: 'error', message: err instanceof Error ? err.message : t('content_filter.delete_failed') });
    }
  };

  const handleRunTest = async () => {
    if (!testText.trim()) return;
    setTesting(true);
    try {
      const res = await testContentFilter({
        text: testText,
        model: testModel,
      });
      setTestResult(res);
    } catch (err) {
      setNotice({ kind: 'error', message: err instanceof Error ? err.message : 'Filter test failed' });
    } finally {
      setTesting(false);
    }
  };

  const applyPreset = async (presetType: 'finance' | 'medical' | 'general') => {
    try {
      if (presetType === 'finance') {
        await createContentFilterRule({
          name: '金融合规防数据泄漏规则 (预设)',
          description: '保护银行卡、支付密码、证券与资金安全敏感词',
          scenario: 'finance',
          action: 'mask',
          pii_types: ['bank_card', 'phone', 'id_card'],
          sensitive_words: ['银行卡号', '信用卡CVV', '支付密码', '交易密码', '证券账号', '资金密码', '客户洗钱', '内幕交易'],
          models: ['*'],
          priority: 30,
        });
      } else if (presetType === 'medical') {
        await createContentFilterRule({
          name: '医疗健康隐私合规规则 (预设)',
          description: '保护患者就诊卡、处方、医保编号及重大疾病等敏感隐私数据',
          scenario: 'medical',
          action: 'mask',
          pii_types: ['medical_record', 'phone', 'id_card'],
          sensitive_words: ['艾滋病确诊', '恶性肿瘤晚期', '精神分裂症病历', '传染病隔离', '阳性诊断书', '处方用药剂量'],
          models: ['*'],
          priority: 30,
        });
      } else {
        await createContentFilterRule({
          name: '通用个人隐私(PII)脱敏规则 (预设)',
          description: '自动检测并掩码手机号、身份证、邮箱、银行卡等个人隐私信息',
          scenario: 'general',
          action: 'mask',
          pii_types: ['phone', 'id_card', 'email', 'bank_card', 'passport'],
          sensitive_words: ['绝密文件', '商业机密', '内部机密', 'root密码'],
          models: ['*'],
          priority: 10,
        });
      }
      setNotice({ kind: 'success', message: t('content_filter.rule_saved') });
      void loadRules();
    } catch (err) {
      setNotice({ kind: 'error', message: err instanceof Error ? err.message : t('content_filter.save_failed') });
    }
  };

  return (
    <div className={styles.pageShell}>
      <div className={styles.pageFrame}>
        {/* Header */}
        <div className={styles.header}>
          <div>
            <h2 className={styles.title}>{t('content_filter.title')}</h2>
            <p className={styles.subtitle}>{t('content_filter.subtitle')}</p>
          </div>
          <div className={styles.headerActions}>
            <div className={styles.navSwitcher}>
              <button
                type="button"
                className={styles.navPill}
                onClick={() => {
                  window.location.href = appPath('/');
                }}
              >
                {t('content_filter.back_to_dashboard')}
              </button>
              <button
                type="button"
                className={styles.navPill}
                onClick={() => {
                  window.location.href = appPath('/users');
                }}
              >
                {t('users.title')}
              </button>
              <button type="button" className={`${styles.navPill} ${styles.navPillActive}`}>
                {t('content_filter.title')}
              </button>
            </div>
            <Button
              onClick={() => {
                setEditingRule(null);
                setShowRuleModal(true);
              }}
            >
              {t('content_filter.new_rule')}
            </Button>
          </div>
        </div>

        {/* Notice alert */}
        {notice && (
          <div className={`${styles.notice} ${notice.kind === 'success' ? styles.noticeSuccess : styles.noticeError}`} role="status">
            {notice.message}
          </div>
        )}

        {/* Tab Navigation */}
        <div className={styles.tabsBar}>
          <button
            type="button"
            className={`${styles.tabBtn} ${activeTab === 'rules' ? styles.tabBtnActive : ''}`}
            onClick={() => setActiveTab('rules')}
          >
            🛡️ {t('content_filter.tab_rules')} ({rules.length})
          </button>
          <button
            type="button"
            className={`${styles.tabBtn} ${activeTab === 'words' ? styles.tabBtnActive : ''}`}
            onClick={() => setActiveTab('words')}
          >
            📖 {t('content_filter.tab_words')}
          </button>
          <button
            type="button"
            className={`${styles.tabBtn} ${activeTab === 'logs' ? styles.tabBtnActive : ''}`}
            onClick={() => setActiveTab('logs')}
          >
            📋 {t('content_filter.tab_logs')}
          </button>
          <button
            type="button"
            className={`${styles.tabBtn} ${activeTab === 'playground' ? styles.tabBtnActive : ''}`}
            onClick={() => setActiveTab('playground')}
          >
            ⚡ {t('content_filter.tab_playground')}
          </button>
        </div>

        {/* Tab 1: Rules List */}
        {activeTab === 'rules' && (
          <>
            <div className={styles.presetsBar}>
              <span className={styles.presetsTitle}>✨ {t('content_filter.rule_presets')}:</span>
              <Button variant="ghost" size="sm" onClick={() => void applyPreset('finance')}>
                🏦 {t('content_filter.apply_preset_finance')}
              </Button>
              <Button variant="ghost" size="sm" onClick={() => void applyPreset('medical')}>
                🏥 {t('content_filter.apply_preset_medical')}
              </Button>
              <Button variant="ghost" size="sm" onClick={() => void applyPreset('general')}>
                🔒 {t('content_filter.apply_preset_general')}
              </Button>
            </div>

            <div className={styles.tableContainer}>
              {rulesLoading ? (
                <div style={{ padding: 48, textAlign: 'center' }}>
                  <LoadingSpinner size={20} />
                </div>
              ) : (
                <table className={styles.table}>
                  <thead>
                    <tr>
                      <th>{t('content_filter.rule_name')}</th>
                      <th>{t('content_filter.scenario')}</th>
                      <th>{t('content_filter.action')}</th>
                      <th>{t('content_filter.pii_types')}</th>
                      <th>{t('content_filter.sensitive_words')}</th>
                      <th>{t('content_filter.models')}</th>
                      <th>{t('content_filter.status')}</th>
                      <th>{t('content_filter.actions')}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {rules.length === 0 && (
                      <tr>
                        <td colSpan={8} className={styles.emptyCell}>
                          {t('content_filter.empty_rules')}
                        </td>
                      </tr>
                    )}
                    {rules.map((r) => {
                      const scenarioClass =
                        r.scenario === 'finance'
                          ? styles.scenarioFinance
                          : r.scenario === 'medical'
                          ? styles.scenarioMedical
                          : r.scenario === 'custom'
                          ? styles.scenarioCustom
                          : styles.scenarioGeneral;

                      const actionClass =
                        r.action === 'block'
                          ? styles.actionBlock
                          : r.action === 'redact'
                          ? styles.actionRedact
                          : styles.actionMask;

                      return (
                        <tr key={r.id}>
                          <td>
                            <strong>{r.name}</strong>
                            {r.description && <div style={{ fontSize: 12, color: 'var(--text-muted)' }}>{r.description}</div>}
                          </td>
                          <td>
                            <span className={`${styles.badge} ${scenarioClass}`}>
                              {t(`content_filter.scenario_${r.scenario}`)}
                            </span>
                          </td>
                          <td>
                            <span className={`${styles.badge} ${actionClass}`}>
                              {t(`content_filter.action_${r.action}`)}
                            </span>
                          </td>
                          <td>
                            <div className={styles.tagList}>
                              {r.pii_types && r.pii_types.length > 0 ? (
                                r.pii_types.map((p) => (
                                  <span key={p} className={styles.tag}>
                                    {t(`content_filter.pii_${p}`, { defaultValue: p })}
                                  </span>
                                ))
                              ) : (
                                <span style={{ color: 'var(--text-muted)', fontSize: 12 }}>-</span>
                              )}
                            </div>
                          </td>
                          <td>
                            {r.sensitive_words && r.sensitive_words.length > 0 ? (
                              <span className={styles.tag}>
                                {r.sensitive_words.length} {t('content_filter.sensitive_words')}
                              </span>
                            ) : (
                              <span style={{ color: 'var(--text-muted)', fontSize: 12 }}>-</span>
                            )}
                          </td>
                          <td>
                            <code style={{ fontSize: 12 }}>
                              {r.models && r.models.length > 0 ? r.models.join(', ') : '*'}
                            </code>
                          </td>
                          <td>
                            <label className={styles.toggleSwitch}>
                              <input
                                type="checkbox"
                                checked={r.enabled}
                                onChange={() => void handleToggleRule(r)}
                              />
                              <span className={styles.toggleSlider} />
                            </label>
                          </td>
                          <td>
                            <div className={styles.actions}>
                              <Button
                                variant="ghost"
                                size="sm"
                                onClick={() => {
                                  setEditingRule(r);
                                  setShowRuleModal(true);
                                }}
                              >
                                {t('common.edit')}
                              </Button>
                              <Button
                                variant="danger"
                                size="sm"
                                onClick={() => void handleDeleteRule(r)}
                              >
                                {t('common.delete')}
                              </Button>
                            </div>
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              )}
            </div>
          </>
        )}

        {/* Tab 2: Sensitive Words Library */}
        {activeTab === 'words' && (
          <div className={styles.playgroundGrid}>
            <div className={styles.card}>
              <h3 className={styles.cardTitle}>🏦 {t('content_filter.scenario_finance')} 敏感词库</h3>
              <p className={styles.formHint}>涵盖银行卡、支付安全、内幕交易、资金安全等高风险合规词汇</p>
              <div className={styles.tagList}>
                {rules
                  .filter((r) => r.scenario === 'finance')
                  .flatMap((r) => r.sensitive_words || [])
                  .map((w, idx) => (
                    <span key={idx} className={styles.tag}>
                      {w}
                    </span>
                  ))}
              </div>
            </div>

            <div className={styles.card}>
              <h3 className={styles.cardTitle}>🏥 {t('content_filter.scenario_medical')} 敏感词库</h3>
              <p className={styles.formHint}>涵盖重大疾病隐私、诊断结论、基因检测、处方隐私等医疗隐私词汇</p>
              <div className={styles.tagList}>
                {rules
                  .filter((r) => r.scenario === 'medical')
                  .flatMap((r) => r.sensitive_words || [])
                  .map((w, idx) => (
                    <span key={idx} className={styles.tag}>
                      {w}
                    </span>
                  ))}
              </div>
            </div>

            <div className={styles.card}>
              <h3 className={styles.cardTitle}>🔒 {t('content_filter.scenario_general')} / 自定义敏感词</h3>
              <p className={styles.formHint}>企业绝密文件、商业机密、系统密钥等</p>
              <div className={styles.tagList}>
                {rules
                  .filter((r) => r.scenario === 'general' || r.scenario === 'custom')
                  .flatMap((r) => r.sensitive_words || [])
                  .map((w, idx) => (
                    <span key={idx} className={styles.tag}>
                      {w}
                    </span>
                  ))}
              </div>
            </div>

            <div className={styles.card}>
              <h3 className={styles.cardTitle}>⚡ 快速添加敏感词至现有规则</h3>
              <Button
                onClick={() => {
                  setEditingRule(null);
                  setShowRuleModal(true);
                }}
              >
                + {t('content_filter.new_rule')}
              </Button>
            </div>
          </div>
        )}

        {/* Tab 3: Logs */}
        {activeTab === 'logs' && (
          <>
            <div className={styles.filterControls}>
              <div style={{ width: 180 }}>
                <Select
                  value={logFilterType}
                  options={[
                    { value: '', label: '全部过滤类型' },
                    { value: 'sensitive_word', label: '敏感词 (Sensitive Word)' },
                    { value: 'pii', label: '个人隐私 (PII)' },
                    { value: 'combined', label: '混合命中 (Combined)' },
                  ]}
                  onChange={setLogFilterType}
                  ariaLabel="过滤类型"
                />
              </div>
              <div style={{ width: 180 }}>
                <Select
                  value={logAction}
                  options={[
                    { value: '', label: '全部执行动作' },
                    { value: 'mask', label: '掩码脱敏 (Mask)' },
                    { value: 'redact', label: '占位替换 (Redact)' },
                    { value: 'block', label: '直接阻断 (Block)' },
                  ]}
                  onChange={setLogAction}
                  ariaLabel="执行动作"
                />
              </div>
              <Button variant="ghost" onClick={() => void loadLogs()}>
                🔄 刷新
              </Button>
              <span style={{ marginLeft: 'auto', fontSize: 13, color: 'var(--text-muted)' }}>
                共 {logsTotal} 条审计记录
              </span>
            </div>

            <div className={styles.tableContainer}>
              {logsLoading ? (
                <div style={{ padding: 48, textAlign: 'center' }}>
                  <LoadingSpinner size={20} />
                </div>
              ) : (
                <table className={styles.table}>
                  <thead>
                    <tr>
                      <th>{t('content_filter.created_at')}</th>
                      <th>{t('content_filter.rule_name')}</th>
                      <th>{t('content_filter.filter_type')}</th>
                      <th>{t('content_filter.action')}</th>
                      <th>{t('content_filter.match_count')}</th>
                      <th>{t('content_filter.raw_preview')}</th>
                      <th>{t('content_filter.filtered_preview')}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {logs.length === 0 && (
                      <tr>
                        <td colSpan={7} className={styles.emptyCell}>
                          {t('content_filter.empty_logs')}
                        </td>
                      </tr>
                    )}
                    {logs.map((log) => (
                      <tr key={log.id}>
                        <td style={{ whiteSpace: 'nowrap', fontSize: 12 }}>
                          {new Date(log.created_at).toLocaleString()}
                        </td>
                        <td>
                          <strong>{log.rule_name || '-'}</strong>
                          {log.model && <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>模型: {log.model}</div>}
                        </td>
                        <td>
                          <span className={styles.tag}>{log.filter_type}</span>
                        </td>
                        <td>
                          <span
                            className={`${styles.badge} ${
                              log.action === 'block'
                                ? styles.actionBlock
                                : log.action === 'redact'
                                ? styles.actionRedact
                                : styles.actionMask
                            }`}
                          >
                            {t(`content_filter.action_${log.action}`, { defaultValue: log.action })}
                          </span>
                        </td>
                        <td>{log.match_count}</td>
                        <td>
                          <div className={styles.logSnippet} title={log.raw_preview}>
                            {log.raw_preview || '-'}
                          </div>
                        </td>
                        <td>
                          <div className={styles.logSnippet} title={log.filtered_preview}>
                            {log.filtered_preview || '-'}
                          </div>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </div>
          </>
        )}

        {/* Tab 4: Live Playground */}
        {activeTab === 'playground' && (
          <div className={styles.playgroundGrid}>
            <div className={styles.card}>
              <h3 className={styles.cardTitle}>🧪 输入待测试文本</h3>
              <div className={styles.sampleButtons}>
                <button
                  type="button"
                  className={styles.sampleBtn}
                  onClick={() =>
                    setTestText(
                      '用户手机 13812345678, 邮箱 user@finance.com, 请将款项转入银行卡 6222021234567890123, 密码为支付密码。'
                    )
                  }
                >
                  样例1: 金融与银行卡
                </button>
                <button
                  type="button"
                  className={styles.sampleBtn}
                  onClick={() =>
                    setTestText(
                      '患者身份证 310101199505051234, 就诊卡号 MZ20230819001, 医保卡 YB9988776655, 诊断包含艾滋病确诊与精神分裂症病历。'
                    )
                  }
                >
                  样例2: 医疗隐私
                </button>
                <button
                  type="button"
                  className={styles.sampleBtn}
                  onClick={() =>
                    setTestText(
                      '绝密文件：请勿外发商业机密, 联系电话 +86 13912345678 或者 admin@company.cn。'
                    )
                  }
                >
                  样例3: 通用PII与机密
                </button>
              </div>

              <textarea
                className={styles.textarea}
                value={testText}
                onChange={(e) => setTestText(e.target.value)}
                placeholder={t('content_filter.test_placeholder')}
              />

              <div style={{ display: 'flex', gap: 10, alignItems: 'center' }}>
                <div style={{ width: 160 }}>
                  <Input
                    label="模拟模型"
                    value={testModel}
                    onChange={(e) => setTestModel(e.target.value)}
                  />
                </div>
                <div style={{ marginTop: 'auto' }}>
                  <Button onClick={() => void handleRunTest()} loading={testing}>
                    🚀 {t('content_filter.test_run')}
                  </Button>
                </div>
              </div>
            </div>

            <div className={styles.card}>
              <h3 className={styles.cardTitle}>✨ {t('content_filter.test_result')}</h3>

              {testResult ? (
                <>
                  <div className={styles.statsRow}>
                    <div className={styles.statItem}>
                      <span className={styles.statLabel}>{t('content_filter.match_count')}</span>
                      <span className={styles.statValue}>{testResult.match_count}</span>
                    </div>
                    <div className={styles.statItem}>
                      <span className={styles.statLabel}>{t('content_filter.status')}</span>
                      <span
                        className={styles.statValue}
                        style={{ color: testResult.blocked ? '#ef4444' : testResult.changed ? '#3b82f6' : '#10b981' }}
                      >
                        {testResult.blocked ? '已阻断 (Blocked)' : testResult.changed ? '已脱敏 (Masked)' : '正常放行'}
                      </span>
                    </div>
                  </div>

                  {testResult.blocked && (
                    <div className={`${styles.notice} ${styles.noticeError}`}>
                      ⛔ {testResult.block_reason || t('content_filter.blocked_notice')}
                    </div>
                  )}

                  {testResult.matched_words.length > 0 && (
                    <div>
                      <span className={styles.formLabel}>{t('content_filter.detected_words')}:</span>
                      <div className={styles.tagList} style={{ marginTop: 4 }}>
                        {testResult.matched_words.map((w, i) => (
                          <span key={i} className={`${styles.tag} ${styles.actionBlock}`}>
                            {w}
                          </span>
                        ))}
                      </div>
                    </div>
                  )}

                  {testResult.matched_pii.length > 0 && (
                    <div>
                      <span className={styles.formLabel}>{t('content_filter.detected_pii')}:</span>
                      <div className={styles.tagList} style={{ marginTop: 4 }}>
                        {testResult.matched_pii.map((p, i) => (
                          <span key={i} className={`${styles.tag} ${styles.actionMask}`}>
                            {t(`content_filter.pii_${p}`, { defaultValue: p })}
                          </span>
                        ))}
                      </div>
                    </div>
                  )}

                  <div>
                    <span className={styles.formLabel}>{t('content_filter.filtered_preview')}:</span>
                    <div className={styles.resultPanel}>{testResult.filtered_text}</div>
                  </div>
                </>
              ) : (
                <div style={{ color: 'var(--text-muted)', textAlign: 'center', padding: 48 }}>
                  点击左侧「执行测试」按钮即可在此查看脱敏结果与命中分析。
                </div>
              )}
            </div>
          </div>
        )}

        {/* Modal: Create or Edit Rule */}
        {showRuleModal && (
          <RuleModal
            rule={editingRule}
            onClose={() => {
              setShowRuleModal(false);
              setEditingRule(null);
            }}
            onSaved={() => {
              setShowRuleModal(false);
              setEditingRule(null);
              setNotice({ kind: 'success', message: t('content_filter.rule_saved') });
              void loadRules();
            }}
          />
        )}
      </div>
    </div>
  );
}

function RuleModal({
  rule,
  onClose,
  onSaved,
}: {
  rule: ContentFilterRule | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useTranslation();
  const [name, setName] = useState(rule?.name ?? '');
  const [description, setDescription] = useState(rule?.description ?? '');
  const [scenario, setScenario] = useState<FilterScenario>(rule?.scenario ?? 'general');
  const [action, setAction] = useState<FilterAction>(rule?.action ?? 'mask');
  const [enabled, setEnabled] = useState(rule?.enabled ?? true);
  const [piiTypes, setPiiTypes] = useState<string[]>(rule?.pii_types ?? ['phone', 'id_card', 'email']);
  const [sensitiveWordsStr, setSensitiveWordsStr] = useState(rule?.sensitive_words?.join('\n') ?? '');
  const [whiteListStr, setWhiteListStr] = useState(rule?.white_list?.join('\n') ?? '');
  const [modelsStr, setModelsStr] = useState(rule?.models?.join(', ') ?? '*');
  const [priority, setPriority] = useState(rule?.priority ?? 10);
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState('');

  const togglePii = (key: string) => {
    setPiiTypes((prev) => (prev.includes(key) ? prev.filter((k) => k !== key) : [...prev, key]));
  };

  const handleSubmit = async (e: FormEvent) => {
    e.preventDefault();
    if (!name.trim()) {
      setErr(t('content_filter.rule_name') + ' is required');
      return;
    }

    setSaving(true);
    setErr('');

    const words = sensitiveWordsStr
      .split(/[\n,]/)
      .map((s) => s.trim())
      .filter(Boolean);

    const whitelist = whiteListStr
      .split(/[\n,]/)
      .map((s) => s.trim())
      .filter(Boolean);

    const models = modelsStr
      .split(/[\n,]/)
      .map((s) => s.trim())
      .filter(Boolean);

    try {
      if (rule) {
        await updateContentFilterRule(rule.id, {
          name,
          description,
          scenario,
          action,
          enabled,
          pii_types: piiTypes,
          sensitive_words: words,
          white_list: whitelist,
          models: models.length > 0 ? models : ['*'],
          priority,
        });
      } else {
        await createContentFilterRule({
          name,
          description,
          scenario,
          action,
          enabled,
          pii_types: piiTypes,
          sensitive_words: words,
          white_list: whitelist,
          models: models.length > 0 ? models : ['*'],
          priority,
        });
      }
      onSaved();
    } catch (error) {
      setErr(error instanceof Error ? error.message : t('content_filter.save_failed'));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Modal
      open
      onClose={onClose}
      title={rule ? t('content_filter.edit_rule') : t('content_filter.new_rule')}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={saving}>
            {t('common.cancel')}
          </Button>
          <Button type="submit" form="rule-form" loading={saving}>
            {t('common.save')}
          </Button>
        </>
      }
    >
      <form id="rule-form" className={styles.form} onSubmit={(e) => void handleSubmit(e)}>
        {err && <div className={styles.noticeError}>{err}</div>}

        <Input
          label={t('content_filter.rule_name')}
          value={name}
          onChange={(e) => setName(e.target.value)}
          required
        />

        <Input
          label="规则描述"
          value={description}
          onChange={(e) => setDescription(e.target.value)}
        />

        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12 }}>
          <div>
            <span className={styles.formLabel}>{t('content_filter.scenario')}</span>
            <div style={{ marginTop: 4 }}>
              <Select
                value={scenario}
                options={SCENARIO_OPTIONS.map((o) => ({ value: o.value, label: t(o.labelKey) }))}
                onChange={(val) => setScenario(val as FilterScenario)}
                ariaLabel={t('content_filter.scenario')}
              />
            </div>
          </div>

          <div>
            <span className={styles.formLabel}>{t('content_filter.action')}</span>
            <div style={{ marginTop: 4 }}>
              <Select
                value={action}
                options={ACTION_OPTIONS.map((o) => ({ value: o.value, label: t(o.labelKey) }))}
                onChange={(val) => setAction(val as FilterAction)}
                ariaLabel={t('content_filter.action')}
              />
            </div>
          </div>
        </div>

        <div>
          <span className={styles.formLabel}>{t('content_filter.pii_types')}</span>
          <div className={styles.checkboxGrid} style={{ marginTop: 6 }}>
            {PII_TYPE_OPTIONS.map((opt) => (
              <label key={opt.key} className={styles.checkboxItem}>
                <input
                  type="checkbox"
                  checked={piiTypes.includes(opt.key)}
                  onChange={() => togglePii(opt.key)}
                />
                <span>{t(opt.labelKey)}</span>
              </label>
            ))}
          </div>
        </div>

        <div className={styles.formField}>
          <label className={styles.formLabel}>{t('content_filter.sensitive_words')}</label>
          <span className={styles.formHint}>{t('content_filter.sensitive_words_hint')}</span>
          <textarea
            className={styles.textarea}
            style={{ minHeight: 80 }}
            value={sensitiveWordsStr}
            onChange={(e) => setSensitiveWordsStr(e.target.value)}
            placeholder="例如：支付密码&#10;商业机密&#10;银行卡号"
          />
        </div>

        <div className={styles.formField}>
          <label className={styles.formLabel}>{t('content_filter.white_list')}</label>
          <span className={styles.formHint}>{t('content_filter.white_list_hint')}</span>
          <textarea
            className={styles.textarea}
            style={{ minHeight: 60 }}
            value={whiteListStr}
            onChange={(e) => setWhiteListStr(e.target.value)}
            placeholder="例如：admin@example.com, 127.0.0.1"
          />
        </div>

        <div style={{ display: 'grid', gridTemplateColumns: '2fr 1fr', gap: 12 }}>
          <Input
            label={t('content_filter.models')}
            hint={t('content_filter.models_hint')}
            value={modelsStr}
            onChange={(e) => setModelsStr(e.target.value)}
          />
          <Input
            label={t('content_filter.priority')}
            type="number"
            value={String(priority)}
            onChange={(e) => setPriority(parseInt(e.target.value, 10) || 0)}
          />
        </div>

        <label className={styles.checkboxItem} style={{ marginTop: 4 }}>
          <input
            type="checkbox"
            checked={enabled}
            onChange={(e) => setEnabled(e.target.checked)}
          />
          <span>{t('content_filter.enabled')}</span>
        </label>
      </form>
    </Modal>
  );
}
