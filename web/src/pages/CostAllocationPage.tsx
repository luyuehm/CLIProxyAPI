import { useCallback, useEffect, useMemo, useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { ApiError, appPath, costAllocationExportURL, fetchCostAllocationDepartments, fetchCostAllocationReport, fetchCostAllocationRules, createCostAllocationRule, updateCostAllocationRule, deleteCostAllocationRule } from '@/lib/api';
import type { CostAllocationDimension, CostAllocationMatchType, CostAllocationRule, CostAllocationRuleCreateRequest, CostAllocationRuleUpdateRequest, CostAllocationReport, DepartmentCostView, DepartmentsResponse } from '@/lib/types';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { Select } from '@/components/ui/Select';
import { Card } from '@/components/ui/Card';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { Modal } from '@/components/ui/Modal';
import styles from './CostAllocationPage.module.scss';

const DIMENSION_OPTIONS = [
  { value: 'department', labelKey: 'costAllocation.dimension_department' },
  { value: 'team', labelKey: 'costAllocation.dimension_team' },
  { value: 'project', labelKey: 'costAllocation.dimension_project' },
];

const MATCH_TYPE_OPTIONS = [
  { value: 'api_key', labelKey: 'costAllocation.match_api_key' },
  { value: 'label', labelKey: 'costAllocation.match_label' },
];

export function CostAllocationPage({ onAuthRequired }: { onAuthRequired?: () => void }) {
  const { t } = useTranslation();
  const [activeTab, setActiveTab] = useState<'departments' | 'rules'>('departments');
  const [dimension, setDimension] = useState('department');
  const [data, setData] = useState<DepartmentsResponse | null>(null);
  const [report, setReport] = useState<CostAllocationReport | null>(null);
  const [rules, setRules] = useState<CostAllocationRule[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState<{ kind: 'success' | 'error'; message: string } | null>(null);
  const [showRuleModal, setShowRuleModal] = useState(false);
  const [editingRule, setEditingRule] = useState<CostAllocationRule | null>(null);
  const [deletingRule, setDeletingRule] = useState<CostAllocationRule | null>(null);

  const loadData = useCallback(async (dim: string) => {
    setLoading(true);
    setError('');
    try {
      if (activeTab === 'departments') {
        const [depts, rpt] = await Promise.all([
          fetchCostAllocationDepartments(dim),
          fetchCostAllocationReport(dim),
        ]);
        setData(depts);
        setReport(rpt);
      } else {
        const r = await fetchCostAllocationRules();
        setRules(r);
      }
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        onAuthRequired?.();
        return;
      }
      setError(err instanceof Error ? err.message : t('costAllocation.load_failed'));
    } finally {
      setLoading(false);
    }
  }, [activeTab, onAuthRequired, t]);

  useEffect(() => {
    void loadData(dimension);
  }, [loadData, dimension]);

  useEffect(() => {
    if (!notice) return;
    const timer = window.setTimeout(() => setNotice(null), 4000);
    return () => window.clearTimeout(timer);
  }, [notice]);

  const dimensionOptions = DIMENSION_OPTIONS.map(({ value, labelKey }) => ({
    value,
    label: t(labelKey),
  }));

  const totalCost = data?.total_cost ?? 0;

  const handleDimensionChange = (dim: string) => {
    setDimension(dim);
  };

  const handleTabChange = (tab: 'departments' | 'rules') => {
    setActiveTab(tab);
  };

  const handleRuleSaved = () => {
    setShowRuleModal(false);
    setEditingRule(null);
    setNotice({ kind: 'success', message: t('costAllocation.rule_saved') });
    void loadData(dimension);
  };

  const handleRuleDeleted = () => {
    setDeletingRule(null);
    setNotice({ kind: 'success', message: t('costAllocation.rule_deleted') });
    void loadData(dimension);
  };

  return (
    <div className={styles.pageShell}>
      <div className={styles.pageFrame}>
        <div className={styles.header}>
          <div>
            <h2 className={styles.title}>{t('costAllocation.title')}</h2>
            <p className={styles.subtitle}>{t('costAllocation.subtitle')}</p>
          </div>
          <div className={styles.headerActions}>
            <Button variant="ghost" onClick={() => { window.location.href = appPath('/'); }}>
              {t('costAllocation.back_to_dashboard')}
            </Button>
            {activeTab === 'rules' && (
              <Button onClick={() => { setEditingRule(null); setShowRuleModal(true); }}>
                {t('costAllocation.new_rule')}
              </Button>
            )}
          </div>
        </div>

        <div className={styles.tabBar}>
          <button
            className={`${styles.tab} ${activeTab === 'departments' ? styles.tabActive : ''}`}
            onClick={() => handleTabChange('departments')}
          >
            {t('costAllocation.report_tab')}
          </button>
          <button
            className={`${styles.tab} ${activeTab === 'rules' ? styles.tabActive : ''}`}
            onClick={() => handleTabChange('rules')}
          >
            {t('costAllocation.rules_tab')}
          </button>
        </div>

        <div className={styles.controls}>
          <span className={styles.controlLabel}>{t('costAllocation.dimension_label')}</span>
          <Select
            value={dimension}
            options={dimensionOptions}
            onChange={handleDimensionChange}
            ariaLabel={t('costAllocation.dimension_label')}
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
        ) : activeTab === 'departments' ? (
          <DepartmentsView
            data={data}
            report={report}
            totalCost={totalCost}
            dimension={dimension}
            t={t}
          />
        ) : (
          <RulesView
            rules={rules}
            dimension={dimension}
            onEdit={(rule) => { setEditingRule(rule); setShowRuleModal(true); }}
            onDelete={(rule) => setDeletingRule(rule)}
            t={t}
          />
        )}
      </div>

      {showRuleModal && (
        <RuleModal
          rule={editingRule}
          onClose={() => { setShowRuleModal(false); setEditingRule(null); }}
          onSaved={handleRuleSaved}
          onAuthRequired={onAuthRequired}
          t={t}
        />
      )}

      {deletingRule && (
        <DeleteRuleModal
          rule={deletingRule}
          onClose={() => setDeletingRule(null)}
          onDeleted={handleRuleDeleted}
          onAuthRequired={onAuthRequired}
          t={t}
        />
      )}
    </div>
  );
}

interface DepartmentsViewProps {
  data: DepartmentsResponse | null;
  report: CostAllocationReport | null;
  totalCost: number;
  dimension: string;
  t: (key: string) => string;
}

function DepartmentsView({ data, report, totalCost, dimension, t }: DepartmentsViewProps) {
  const depts = data?.departments ?? [];

  if (totalCost === 0 && !data?.unassigned_requests) {
    return (
      <Card className={styles.card}>
        <div className={styles.emptyState}>{t('costAllocation.report_empty')}</div>
      </Card>
    );
  }

  return (
    <div className={styles.content}>
      <Card title={t('costAllocation.departments_card')} className={styles.card}>
        <div className={styles.usageSummary}>
          <div className={styles.usageStat}>
            <span className={styles.usageStatLabel}>{t('costAllocation.total_cost')}</span>
            <span className={styles.usageStatValue}>{formatUSD(totalCost)}</span>
          </div>
          {data && data.unassigned_cost > 0 && (
            <div className={styles.usageStat}>
              <span className={styles.usageStatLabel}>{t('costAllocation.unassigned')}</span>
              <span className={styles.usageStatValue}>{formatUSD(data.unassigned_cost)}</span>
            </div>
          )}
        </div>

        {data && !data.cost_available && (
          <div className={styles.hint}>{t('costAllocation.cost_unavailable_hint')}</div>
        )}

        {depts.map((dept) => (
          <DepartmentRow key={dept.name} dept={dept} totalCost={totalCost} t={t} />
        ))}

        {data && data.unassigned_cost > 0 && (
          <div className={styles.departmentBar}>
            <div className={styles.departmentHeader}>
              <span className={styles.departmentName}>{t('costAllocation.unassigned')}</span>
              <span className={styles.departmentCost}>{formatUSD(data.unassigned_cost)}</span>
            </div>
            <div className={styles.departmentStats}>
              <span>{data.unassigned_requests.toLocaleString()} {t('costAllocation.requests')}</span>
            </div>
            <div className={styles.progressBarContainer}>
              <div
                className={`${styles.progressBar} ${styles.progressBarUnassigned}`}
                style={{ width: `${totalCost > 0 ? (data.unassigned_cost / totalCost) * 100 : 0}%` }}
              />
              <span className={styles.progressLabel}>
                {totalCost > 0 ? ((data.unassigned_cost / totalCost) * 100).toFixed(1) : '0'}%
              </span>
            </div>
            <div className={styles.hint}>{t('costAllocation.unassigned_hint')}</div>
          </div>
        )}
      </Card>

      {report && report.items.length > 0 && (
        <Card title={t('costAllocation.report_card')} className={styles.card}>
          <div className={styles.cardHeader}>
            <Button
              variant="ghost"
              onClick={() => { window.open(costAllocationExportURL(dimension), '_blank'); }}
            >
              {t('costAllocation.export_csv')}
            </Button>
          </div>
          <div className={styles.tableContainer}>
            <table className={styles.table}>
              <thead>
                <tr>
                  <th>{t('costAllocation.rule_name')}</th>
                  <th>{t('costAllocation.model')}</th>
                  <th className={styles.numericCol}>{t('costAllocation.requests')}</th>
                  <th className={styles.numericCol}>{t('costAllocation.tokens')}</th>
                  <th className={styles.numericCol}>{t('costAllocation.cost')}</th>
                  <th className={styles.numericCol}>{t('costAllocation.share')}</th>
                </tr>
              </thead>
              <tbody>
                {report.items.map((item, idx) => (
                  <tr key={idx}>
                    <td>{item.name}</td>
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
    </div>
  );
}

interface DepartmentRowProps {
  dept: DepartmentCostView;
  totalCost: number;
  t: (key: string) => string;
}

function DepartmentRow({ dept, totalCost, t }: DepartmentRowProps) {
  return (
    <div className={styles.departmentBar}>
      <div className={styles.departmentHeader}>
        <span className={styles.departmentName}>{dept.name}</span>
        <span className={styles.departmentCost}>{formatUSD(dept.cost)}</span>
      </div>
      <div className={styles.departmentStats}>
        <span>{dept.requests.toLocaleString()} {t('costAllocation.requests')}</span>
        <span>{dept.total_tokens.toLocaleString()} {t('costAllocation.tokens')}</span>
        <span>{dept.cost_share.toFixed(1)}%</span>
      </div>
      <div className={styles.progressBarContainer}>
        <div
          className={`${styles.progressBar} ${styles.progressBarDepartment}`}
          style={{ width: `${totalCost > 0 ? (dept.cost / totalCost) * 100 : 0}%` }}
        />
        <span className={styles.progressLabel}>
          {totalCost > 0 ? ((dept.cost / totalCost) * 100).toFixed(1) : '0'}%
        </span>
      </div>
    </div>
  );
}

interface RulesViewProps {
  rules: CostAllocationRule[];
  dimension: string;
  onEdit: (rule: CostAllocationRule) => void;
  onDelete: (rule: CostAllocationRule) => void;
  t: (key: string) => string;
}

function RulesView({ rules, onEdit, onDelete, t }: RulesViewProps) {
  if (rules.length === 0) {
    return (
      <Card className={styles.card}>
        <div className={styles.emptyState}>{t('costAllocation.rules_empty')}</div>
      </Card>
    );
  }

  return (
    <Card className={styles.card}>
      {rules.map((rule) => (
        <div key={rule.id} className={`${styles.ruleItem} ${!rule.enabled ? styles.ruleDisabled : ''}`}>
          <div className={styles.ruleInfo}>
            <div className={styles.ruleName}>{rule.name}</div>
            <div className={styles.ruleMeta}>
              <span className={styles.ruleTag}>{rule.dimension}</span>
              <span className={styles.ruleTag}>{rule.match_type}</span>
              <span>{rule.match_values.length} {t('costAllocation.rule_match_values')}</span>
              {rule.priority > 0 && <span>P{rule.priority}</span>}
              {!rule.enabled && <span>({t('costAllocation.rule_enabled')}: false)</span>}
            </div>
          </div>
          <div className={styles.ruleActions}>
            <Button variant="ghost" onClick={() => onEdit(rule)}>
              {t('common.edit')}
            </Button>
            <Button variant="ghost" onClick={() => onDelete(rule)}>
              {t('common.delete')}
            </Button>
          </div>
        </div>
      ))}
    </Card>
  );
}

interface RuleModalProps {
  rule: CostAllocationRule | null;
  onClose: () => void;
  onSaved: () => void;
  onAuthRequired?: () => void;
  t: (key: string) => string;
}

function RuleModal({ rule, onClose, onSaved, onAuthRequired, t }: RuleModalProps) {
  const isEditing = rule !== null;
  const [name, setName] = useState(rule?.name ?? '');
  const [dimension, setDimension] = useState<CostAllocationDimension>(rule?.dimension ?? 'department');
  const [matchType, setMatchType] = useState<CostAllocationMatchType>(rule?.match_type ?? 'api_key');
  const [matchValuesText, setMatchValuesText] = useState(rule?.match_values.join('\n') ?? '');
  const [enabled, setEnabled] = useState(rule?.enabled ?? true);
  const [priority, setPriority] = useState(String(rule?.priority ?? 0));
  const [note, setNote] = useState(rule?.note ?? '');
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState('');

  const dimensionOptions = DIMENSION_OPTIONS.map(({ value, labelKey }) => ({
    value,
    label: t(labelKey),
  }));

  const matchTypeOptions = MATCH_TYPE_OPTIONS.map(({ value, labelKey }) => ({
    value,
    label: t(labelKey),
  }));

  const handleSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setSaving(true);
    setErr('');

    const matchValues = matchValuesText
      .split(/[\n,]+/)
      .map((v) => v.trim())
      .filter((v) => v.length > 0);

    if (matchValues.length === 0) {
      setErr(t('costAllocation.rule_match_values_hint'));
      setSaving(false);
      return;
    }

    try {
      if (isEditing) {
        const payload: CostAllocationRuleUpdateRequest = {
          name: name || undefined,
          dimension: dimension as CostAllocationDimension,
          match_type: matchType as CostAllocationMatchType,
          match_values: matchValues,
          enabled,
          priority: Number(priority) || 0,
          note: note || undefined,
        };
        await updateCostAllocationRule(rule.id, payload);
      } else {
        const payload: CostAllocationRuleCreateRequest = {
          name,
          dimension: dimension as CostAllocationDimension,
          match_type: matchType as CostAllocationMatchType,
          match_values: matchValues,
          enabled,
          priority: Number(priority) || 0,
          note: note || undefined,
        };
        await createCostAllocationRule(payload);
      }
      onSaved();
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) {
        onAuthRequired?.();
        return;
      }
      setErr(error instanceof Error ? error.message : t('costAllocation.rule_save_failed'));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Modal
      open
      title={isEditing ? t('costAllocation.edit_rule_title') : t('costAllocation.create_rule_title')}
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={saving}>
            {t('common.cancel')}
          </Button>
          <Button type="submit" form="rule-form" loading={saving}>
            {saving ? t('common.loading') : t('common.save')}
          </Button>
        </>
      }
    >
      <form id="rule-form" className={styles.form} onSubmit={(event) => void handleSubmit(event)}>
        {err && <div className={styles.errorBox}>{err}</div>}
        <Input
          label={t('costAllocation.rule_name')}
          value={name}
          onChange={(event) => setName(event.target.value)}
          required
        />
        <div className={styles.formRow}>
          <Select
            value={dimension}
            options={dimensionOptions}
            onChange={(v) => setDimension(v as CostAllocationDimension)}
            ariaLabel={t('costAllocation.rule_dimension')}
          />
          <Select
            value={matchType}
            options={matchTypeOptions}
            onChange={(v) => setMatchType(v as CostAllocationMatchType)}
            ariaLabel={t('costAllocation.rule_match_type')}
          />
        </div>
        <div className={styles.multiValueInput}>
          <label className={styles.controlLabel}>{t('costAllocation.rule_match_values')}</label>
          <textarea
            value={matchValuesText}
            onChange={(event) => setMatchValuesText(event.target.value)}
            placeholder={t('costAllocation.rule_match_values_hint')}
          />
          <span className={styles.multiValueHint}>{t('costAllocation.rule_match_values_hint')}</span>
        </div>
        <div className={styles.formRow}>
          <Input
            type="number"
            label={t('costAllocation.rule_priority')}
            value={priority}
            onChange={(event) => setPriority(event.target.value)}
            min={0}
          />
          <Input
            label={t('costAllocation.rule_note')}
            value={note}
            onChange={(event) => setNote(event.target.value)}
          />
        </div>
        <label className={styles.checkboxRow}>
          <input
            type="checkbox"
            checked={enabled}
            onChange={(event) => setEnabled(event.target.checked)}
          />
          <span>{t('costAllocation.rule_enabled')}</span>
        </label>
      </form>
    </Modal>
  );
}

interface DeleteRuleModalProps {
  rule: CostAllocationRule;
  onClose: () => void;
  onDeleted: () => void;
  onAuthRequired?: () => void;
  t: (key: string) => string;
}

function DeleteRuleModal({ rule, onClose, onDeleted, onAuthRequired, t }: DeleteRuleModalProps) {
  const [deleting, setDeleting] = useState(false);
  const [err, setErr] = useState('');

  const handleDelete = async () => {
    setDeleting(true);
    setErr('');
    try {
      await deleteCostAllocationRule(rule.id);
      onDeleted();
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) {
        onAuthRequired?.();
        return;
      }
      setErr(error instanceof Error ? error.message : t('costAllocation.rule_save_failed'));
      setDeleting(false);
    }
  };

  return (
    <Modal
      open
      title={t('costAllocation.delete_rule_title')}
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={deleting}>
            {t('common.cancel')}
          </Button>
          <Button type="submit" loading={deleting} onClick={handleDelete}>
            {deleting ? t('common.loading') : t('common.delete')}
          </Button>
        </>
      }
    >
      <div className={styles.confirmDelete}>
        {err && <div className={styles.errorBox}>{err}</div>}
        <p>{t('costAllocation.delete_rule_confirm')}</p>
        <p><strong>{rule.name}</strong> ({rule.dimension}, {rule.match_type})</p>
      </div>
    </Modal>
  );
}

function formatUSD(value: number): string {
  return `$${value.toFixed(2)}`;
}