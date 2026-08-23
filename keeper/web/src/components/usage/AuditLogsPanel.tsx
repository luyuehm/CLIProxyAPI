import {
  useCallback,
  useEffect,
  useMemo,
  useState,
} from 'react';
import { useTranslation } from 'react-i18next';
import {
  ApiError,
  fetchAuditLogRequestLog,
  fetchAuditLogs,
  type FetchAuditLogsOptions,
} from '@/lib/api';
import type { UsageEvent } from '@/lib/types';
import { Button } from '@/components/ui/Button';
import { Card } from '@/components/ui/Card';
import { EmptyState } from '@/components/ui/EmptyState';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { Select } from '@/components/ui/Select';
import { IconDownload, IconEye, IconX, IconRefreshCw } from '@/components/ui/icons';
import { formatDurationMs, formatUsd } from '@/utils/usage';
import styles from './AuditLogsPanel.module.scss';

const ALL_FILTER = '__all__';
const AUDIT_LOG_PAGE_SIZES = [20, 50, 100, 500] as const;

const statusClassName = (statusCode: number): string => {
  const prefix = Math.floor(statusCode / 100);
  switch (prefix) {
    case 2: return styles.statusSuccess;
    case 3: return styles.statusRedirect;
    case 4: return styles.statusClientError;
    case 5: return styles.statusServerError;
    default: return styles.statusOther;
  }
};

interface AuditLogsPanelProps {
  range: string;
  start?: string;
  end?: string;
  apiKeyId?: string;
}

export function AuditLogsPanel({ range, start, end, apiKeyId }: AuditLogsPanelProps) {
  const { t } = useTranslation();
  const [events, setEvents] = useState<UsageEvent[]>([]);
  const [totalCount, setTotalCount] = useState(0);
  const [totalPages, setTotalPages] = useState(0);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState<number>(100);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [model, setModel] = useState(ALL_FILTER);
  const [provider, setProvider] = useState(ALL_FILTER);
  const [result, setResult] = useState(ALL_FILTER);
  const [statusGroup, setStatusGroup] = useState(ALL_FILTER);
  const [detailEvent, setDetailEvent] = useState<UsageEvent | null>(null);
  const [detailContent, setDetailContent] = useState('');
  const [detailLoading, setDetailLoading] = useState(false);
  const [detailError, setDetailError] = useState('');
  const [exporting, setExporting] = useState(false);

  const modelOptions = useMemo(() => {
    const seen = new Set<string>([ALL_FILTER]);
    const options = [{ value: ALL_FILTER, label: t('audit_logs.filter_all_models') }];
    for (const event of events) {
      const value = event.model?.trim();
      if (!value || seen.has(value)) continue;
      seen.add(value);
      options.push({ value, label: value });
    }
    return options;
  }, [events, t]);

  const providerOptions = useMemo(() => {
    const seen = new Set<string>([ALL_FILTER]);
    const options = [{ value: ALL_FILTER, label: t('audit_logs.filter_all_providers') }];
    for (const event of events) {
      const value = event.source?.trim() || event.api_key?.trim() || '';
      if (!value || seen.has(value)) continue;
      seen.add(value);
      options.push({ value, label: value });
    }
    return options;
  }, [events, t]);

  const statusOptions = useMemo(
    () => [
      { value: ALL_FILTER, label: t('audit_logs.filter_all_statuses') },
      { value: '2xx', label: t('audit_logs.status_2xx') },
      { value: '3xx', label: t('audit_logs.status_3xx') },
      { value: '4xx', label: t('audit_logs.status_4xx') },
      { value: '5xx', label: t('audit_logs.status_5xx') },
    ],
    [t]
  );

  const resultOptions = useMemo(
    () => [
      { value: ALL_FILTER, label: t('audit_logs.filter_all_results') },
      { value: 'success', label: t('audit_logs.result_success') },
      { value: 'failed', label: t('audit_logs.result_failed') },
    ],
    [t]
  );

  const buildOptions = useCallback((): FetchAuditLogsOptions => {
    const opts: FetchAuditLogsOptions = { page, pageSize };
    if (model !== ALL_FILTER) opts.model = model;
    if (provider !== ALL_FILTER) opts.provider = provider;
    if (result !== ALL_FILTER) opts.result = result;
    if (statusGroup !== ALL_FILTER) {
      opts.statusGroup = statusGroup;
    }
    if (apiKeyId) opts.apiKeyId = apiKeyId;
    return opts;
  }, [apiKeyId, model, page, pageSize, provider, result, statusGroup]);

  const loadEvents = useCallback(async () => {
    setLoading(true);
    setError('');
    try {
      const data = await fetchAuditLogs(range, start, end, undefined, buildOptions());
      setEvents(data.events);
      setTotalCount(data.total_count);
      setTotalPages(data.total_pages);
    } catch (err) {
      const message = err instanceof ApiError && err.status === 401
        ? 'AUTH_REQUIRED'
        : (err instanceof Error ? err.message : String(err));
      setError(message);
    } finally {
      setLoading(false);
    }
  }, [buildOptions, range, start, end]);

  useEffect(() => {
    void loadEvents();
  }, [loadEvents, page]);

  const openDetail = useCallback(async (event: UsageEvent) => {
    setDetailEvent(event);
    setDetailContent('');
    setDetailError('');
    const requestId = event.id?.trim();
    if (!requestId) {
      setDetailError(t('audit_logs.no_request_id'));
      return;
    }
    setDetailLoading(true);
    try {
      const content = await fetchAuditLogRequestLog(requestId);
      setDetailContent(content);
    } catch (err) {
      setDetailError(err instanceof Error ? err.message : String(err));
    } finally {
      setDetailLoading(false);
    }
  }, [t]);

  const handleExport = useCallback(async (format: 'csv' | 'json') => {
    setExporting(true);
    setError('');
    try {
      const opts = buildOptions();
      opts.exportFormat = format;
      void await fetchAuditLogs(range, start, end, undefined, opts);
      const data = await fetchAuditLogs(range, start, end, undefined, { ...buildOptions(), pageSize: 1000, page: 1 });
      const blob = new Blob([JSON.stringify(data.events, null, 2)], {
        type: format === 'csv' ? 'text/csv' : 'application/json',
      });
      const url = URL.createObjectURL(blob);
      const link = document.createElement('a');
      link.href = url;
      link.download = `audit-logs.${format}`;
      link.click();
      URL.revokeObjectURL(url);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setExporting(false);
    }
  }, [buildOptions, range, start, end]);

  const applyFilterChange = useCallback((setter: (value: string) => void) => (value: string) => {
    setter(value);
    setPage(1);
  }, []);

  return (
    <Card className={styles.auditLogsCard}>
      <div className={styles.auditLogsHeader}>
        <h3 className={styles.auditLogsTitle}>{t('audit_logs.title')}</h3>
        <div className={styles.auditLogsActions}>
          <Button variant="ghost" size="sm" disabled={exporting || totalCount === 0} onClick={() => void handleExport('csv')}>
            <IconDownload size={14} /> {t('audit_logs.export_csv')}
          </Button>
          <Button variant="ghost" size="sm" disabled={exporting || totalCount === 0} onClick={() => void handleExport('json')}>
            <IconDownload size={14} /> {t('audit_logs.export_json')}
          </Button>
        </div>
      </div>

      <div className={styles.auditLogsFilters}>
        <label className={styles.filterField}>
          <span className={styles.filterLabel}>{t('audit_logs.filter_model')}</span>
          <Select value={model} options={modelOptions} onChange={applyFilterChange(setModel)} fullWidth ariaLabel={t('audit_logs.filter_model')} dropdownMinWidth={160} />
        </label>
        <label className={styles.filterField}>
          <span className={styles.filterLabel}>{t('audit_logs.filter_provider')}</span>
          <Select value={provider} options={providerOptions} onChange={applyFilterChange(setProvider)} fullWidth ariaLabel={t('audit_logs.filter_provider')} dropdownMinWidth={160} />
        </label>
        <label className={styles.filterField}>
          <span className={styles.filterLabel}>{t('audit_logs.filter_result')}</span>
          <Select value={result} options={resultOptions} onChange={applyFilterChange(setResult)} fullWidth ariaLabel={t('audit_logs.filter_result')} dropdownMinWidth={160} />
        </label>
        <label className={styles.filterField}>
          <span className={styles.filterLabel}>{t('audit_logs.filter_status')}</span>
          <Select value={statusGroup} options={statusOptions} onChange={applyFilterChange(setStatusGroup)} fullWidth ariaLabel={t('audit_logs.filter_status')} dropdownMinWidth={160} />
        </label>
        <div className={styles.filterSpacer} />
        <Button variant="ghost" size="sm" onClick={() => void loadEvents()}>
          <IconRefreshCw size={14} /> {t('audit_logs.refresh')}
        </Button>
      </div>

      {error && <div className={styles.errorBox}>{error === 'AUTH_REQUIRED' ? t('auth.session_expired') : error}</div>}

      <div className={styles.auditLogsTableWrap}>
        {loading && events.length === 0 ? (
          <div className={styles.auditLogsLoading}><LoadingSpinner /></div>
        ) : events.length === 0 ? (
          <EmptyState title={t('audit_logs.empty')} />
        ) : (
          <table className={styles.auditLogsTable}>
            <thead>
              <tr>
                <th>{t('audit_logs.column_timestamp')}</th>
                <th>{t('audit_logs.column_user')}</th>
                <th>{t('audit_logs.column_provider')}</th>
                <th>{t('audit_logs.column_model')}</th>
                <th>{t('audit_logs.column_status')}</th>
                <th>{t('audit_logs.column_result')}</th>
                <th>{t('audit_logs.column_duration')}</th>
                <th>{t('audit_logs.column_tokens')}</th>
                <th>{t('audit_logs.column_cost')}</th>
                <th aria-label={t('audit_logs.column_actions')} />
              </tr>
            </thead>
            <tbody>
              {events.map((event) => (
                <tr key={event.id ?? event.timestamp}>
                  <td className={styles.cellTimestamp}>{event.timestamp}</td>
                  <td className={styles.cellUser}>{event.source || event.api_key || '—'}</td>
                  <td className={styles.cellProvider}>{event.source_type || event.source_raw || '—'}</td>
                  <td className={styles.cellModel}>{event.model || '—'}</td>
                  <td className={styles.cellStatus}>
                    <span className={`${styles.statusBadge} ${statusClassName(event.status_code)}`}>{event.status_code || '—'}</span>
                  </td>
                  <td className={styles.cellResult}>{event.failed ? t('audit_logs.result_failed') : t('audit_logs.result_success')}</td>
                  <td className={styles.cellDuration}>{event.latency_ms > 0 ? formatDurationMs(event.latency_ms) : '—'}</td>
                  <td className={styles.cellTokens}>{event.tokens?.total_tokens?.toLocaleString() ?? '—'}</td>
                  <td className={styles.cellCost}>{event.cost_available ? formatUsd(event.cost_usd ?? 0) : '—'}</td>
                  <td className={styles.cellAction}>
                    <button
                      type="button"
                      className={styles.detailButton}
                      onClick={() => void openDetail(event)}
                      title={t('audit_logs.view_detail')}
                    >
                      <IconEye size={14} />
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {totalPages > 1 && (
        <div className={styles.pagination}>
          <span className={styles.paginationTotal}>
            {t('audit_logs.total', { count: totalCount.toLocaleString() })}
          </span>
          <div className={styles.paginationControls}>
            <Button variant="ghost" size="sm" disabled={page <= 1 || loading} onClick={() => setPage((p) => Math.max(1, p - 1))}>
              {t('audit_logs.prev')}
            </Button>
            <span className={styles.paginationPage}>
              {page} / {totalPages}
            </span>
            <Button variant="ghost" size="sm" disabled={page >= totalPages || loading} onClick={() => setPage((p) => Math.min(totalPages, p + 1))}>
              {t('audit_logs.next')}
            </Button>
            <Select
              value={String(pageSize)}
              options={AUDIT_LOG_PAGE_SIZES.map((size) => ({ value: String(size), label: String(size) }))}
              onChange={(value) => {
                setPageSize(Number(value));
                setPage(1);
              }}
              className={styles.pageSizeSelect}
              ariaLabel={t('audit_logs.page_size')}
            />
          </div>
        </div>
      )}

      {detailEvent && (
        <div className={styles.detailOverlay} onClick={() => { setDetailEvent(null); setDetailContent(''); setDetailError(''); }}>
          <div className={styles.detailModal} onClick={(e) => e.stopPropagation()}>
            <div className={styles.detailHeader}>
              <h4>{t('audit_logs.request_detail_title')}</h4>
              <button type="button" className={styles.detailClose} onClick={() => { setDetailEvent(null); setDetailContent(''); setDetailError(''); }} aria-label={t('audit_logs.close')}>
                <IconX size={16} />
              </button>
            </div>
            <div className={styles.detailMeta}>
              <div>
                <span className={styles.detailMetaLabel}>{t('audit_logs.column_model')}</span>
                <span className={styles.detailMetaValue}>{detailEvent.model}</span>
              </div>
              <div>
                <span className={styles.detailMetaLabel}>{t('audit_logs.column_timestamp')}</span>
                <span className={styles.detailMetaValue}>{detailEvent.timestamp}</span>
              </div>
              <div>
                <span className={styles.detailMetaLabel}>{t('audit_logs.column_status')}</span>
                <span className={styles.detailMetaValue}>{detailEvent.status_code}</span>
              </div>
            </div>
            {detailLoading && <div className={styles.detailLoading}><LoadingSpinner /></div>}
            {detailError && <div className={styles.errorBox}>{detailError}</div>}
            {detailContent && <pre className={styles.detailContent}>{detailContent}</pre>}
          </div>
        </div>
      )}
    </Card>
  );
}