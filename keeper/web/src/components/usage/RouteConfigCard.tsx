import { useCallback, useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Card } from '@/components/ui/Card';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { Modal } from '@/components/ui/Modal';
import { IconCircleAlert, IconCheck, IconPlus } from '@/components/ui/icons';
import { ApiError, deleteRouteConfig, fetchRouteConfigs, upsertRouteConfig } from '@/lib/api';
import type { RouteConfig, RouteConfigInput } from '@/lib/types';
import styles from '@/pages/UsagePage.module.scss';

interface RouteConfigCardProps {
  onAuthRequired?: () => void;
  onNotice?: (kind: 'success' | 'info' | 'error', message: string) => void;
}

const emptyForm = (): RouteConfigInput => ({
  model: '',
  base_url: '',
  enabled: true,
  strategy: 'fixed',
  api_key: '',
  weight: 100,
  description: '',
});

export function RouteConfigCard({ onAuthRequired, onNotice }: RouteConfigCardProps) {
  const { t } = useTranslation();
  const [routes, setRoutes] = useState<RouteConfig[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [modalOpen, setModalOpen] = useState(false);
  const [editing, setEditing] = useState<RouteConfig | null>(null);
  const [form, setForm] = useState<RouteConfigInput>(emptyForm());
  const [saving, setSaving] = useState(false);
  const requestControllerRef = useRef<AbortController | null>(null);

  const loadRoutes = useCallback(async () => {
    requestControllerRef.current?.abort();
    const controller = new AbortController();
    requestControllerRef.current = controller;
    setLoading(true);
    setError('');
    try {
      const response = await fetchRouteConfigs(controller.signal);
      if (requestControllerRef.current !== controller) return;
      setRoutes(response.routes ?? []);
    } catch (err) {
      if (controller.signal.aborted) return;
      if (err instanceof ApiError && err.status === 401) {
        onAuthRequired?.();
        return;
      }
      setError(err instanceof Error ? err.message : 'Failed to load routes');
    } finally {
      if (requestControllerRef.current === controller) {
        setLoading(false);
        requestControllerRef.current = null;
      }
    }
  }, [onAuthRequired]);

  useEffect(() => {
    void loadRoutes();
    return () => {
      requestControllerRef.current?.abort();
      requestControllerRef.current = null;
    };
  }, [loadRoutes]);

  const openAdd = useCallback(() => {
    setEditing(null);
    setForm(emptyForm());
    setModalOpen(true);
  }, []);

  const openEdit = useCallback((route: RouteConfig) => {
    setEditing(route);
    setForm({
      model: route.model,
      base_url: route.base_url,
      enabled: route.enabled,
      strategy: route.strategy,
      api_key: route.api_key ?? '',
      weight: route.weight,
      description: route.description ?? '',
    });
    setModalOpen(true);
  }, []);

  const handleSave = useCallback(async () => {
    if (!form.model.trim() || !form.base_url.trim()) return;
    setSaving(true);
    try {
      const pathModel = editing ? editing.model : undefined;
      await upsertRouteConfig(form, pathModel);
      await loadRoutes();
      setModalOpen(false);
      onNotice?.('success', t('route_config.saved'));
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        onAuthRequired?.();
        return;
      }
      onNotice?.('error', err instanceof Error ? err.message : 'Failed to save route');
    } finally {
      setSaving(false);
    }
  }, [editing, form, loadRoutes, onAuthRequired, onNotice, t]);

  const handleDelete = useCallback(async (model: string) => {
    if (!window.confirm(t('route_config.delete_confirm'))) return;
    try {
      await deleteRouteConfig(model);
      await loadRoutes();
      onNotice?.('success', t('route_config.deleted'));
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        onAuthRequired?.();
        return;
      }
      onNotice?.('error', err instanceof Error ? err.message : 'Failed to delete route');
    }
  }, [loadRoutes, onAuthRequired, onNotice, t]);

  return (
    <>
      <Card className={styles.apiKeySettingsCard}>
        <div className={styles.sectionTitleBlock}>
          <h3 className={styles.sectionTitle}>{t('route_config.title')}</h3>
          <p className={styles.sectionSubtitle}>{t('route_config.subtitle')}</p>
        </div>
        {error && <div className={styles.errorBox}>{error}</div>}
        {loading && <p>{t('common.loading')}</p>}
        {!loading && !error && routes.length === 0 && (
          <p>{t('route_config.empty')}</p>
        )}
        {!loading && routes.length > 0 && (
          <table className="route-config-table" style={{ width: '100%', borderCollapse: 'collapse', marginBottom: 12 }}>
            <thead>
              <tr style={{ textAlign: 'left', borderBottom: '1px solid var(--border-color)' }}>
                <th style={{ padding: '8px 4px' }}>{t('route_config.model')}</th>
                <th style={{ padding: '8px 4px' }}>{t('route_config.base_url')}</th>
                <th style={{ padding: '8px 4px' }}>{t('route_config.enabled')}</th>
                <th style={{ padding: '8px 4px' }}>{t('route_config.weight')}</th>
                <th style={{ padding: '8px 4px' }}>{t('route_config.strategy')}</th>
                <th style={{ padding: '8px 4px' }}>{t('common.actions')}</th>
              </tr>
            </thead>
            <tbody>
              {routes.map((route) => (
                <tr key={route.id} style={{ borderBottom: '1px solid var(--border-color)' }}>
                  <td style={{ padding: '8px 4px', fontWeight: 500 }}>{route.model}</td>
                  <td style={{ padding: '8px 4px', maxWidth: 240, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{route.base_url}</td>
                  <td style={{ padding: '8px 4px' }}>{route.enabled ? <IconCheck size={14} /> : '—'}</td>
                  <td style={{ padding: '8px 4px' }}>{route.weight}</td>
                  <td style={{ padding: '8px 4px' }}>{route.strategy}</td>
                  <td style={{ padding: '8px 4px' }}>
                    <Button size="sm" variant="secondary" onClick={() => openEdit(route)} style={{ marginRight: 4 }}>
                      {t('common.edit')}
                    </Button>
                    <Button size="sm" variant="danger" onClick={() => handleDelete(route.model)}>
                      {t('common.delete')}
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        <Button variant="primary" size="sm" onClick={openAdd}>
          <IconPlus size={14} /> {t('route_config.add')}
        </Button>
      </Card>

      <Modal
        open={modalOpen}
        onClose={() => setModalOpen(false)}
        title={editing ? t('route_config.edit_title') : t('route_config.add_title')}
        footer={
          <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
            <Button variant="secondary" onClick={() => setModalOpen(false)}>{t('common.cancel')}</Button>
            <Button variant="primary" onClick={handleSave} loading={saving} disabled={!form.model.trim() || !form.base_url.trim()}>
              {t('common.save')}
            </Button>
          </div>
        }
      >
        <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
          <Input
            label={t('route_config.model')}
            value={form.model}
            onChange={(e) => setForm({ ...form, model: e.target.value })}
            disabled={editing !== null}
            placeholder="gpt-4o"
          />
          <Input
            label={t('route_config.base_url')}
            value={form.base_url}
            onChange={(e) => setForm({ ...form, base_url: e.target.value })}
            placeholder="https://api.example.com/v1"
          />
          <Input
            label={t('route_config.api_key')}
            value={form.api_key ?? ''}
            onChange={(e) => setForm({ ...form, api_key: e.target.value })}
            placeholder="sk-..."
          />
          <div style={{ display: 'flex', gap: 12 }}>
            <label style={{ flex: 1, display: 'flex', alignItems: 'center', gap: 8 }}>
              <input
                type="checkbox"
                checked={form.enabled !== false}
                onChange={(e) => setForm({ ...form, enabled: e.target.checked })}
              />
              {t('route_config.enabled')}
            </label>
            <div style={{ flex: 1 }}>
              <Input
                label={t('route_config.weight')}
                type="number"
                min={0}
                max={100}
                value={String(form.weight ?? 100)}
                onChange={(e) => setForm({ ...form, weight: parseInt(e.target.value) || 0 })}
              />
            </div>
          </div>
          <Input
            label={t('route_config.description')}
            value={form.description ?? ''}
            onChange={(e) => setForm({ ...form, description: e.target.value })}
            placeholder="Optional description"
          />
        </div>
      </Modal>
    </>
  );
}