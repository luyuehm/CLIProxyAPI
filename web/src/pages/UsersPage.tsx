import { useCallback, useEffect, useState, type FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { appPath, createUser, deleteUser, fetchUsers, updateUser } from '@/lib/api';
import { ApiError } from '@/lib/api';
import type { User, UserRole, UserUpdateRequest } from '@/lib/types';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { Modal } from '@/components/ui/Modal';
import { Select } from '@/components/ui/Select';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import styles from './UsersPage.module.scss';

const ROLE_OPTIONS: ReadonlyArray<{ value: UserRole; labelKey: string }> = [
  { value: 'admin', labelKey: 'users.role_admin' },
  { value: 'operator', labelKey: 'users.role_operator' },
  { value: 'viewer', labelKey: 'users.role_viewer' },
];

const ROLE_BADGE_CLASSES: Record<UserRole, string> = {
  admin: `${styles.roleBadge} ${styles.roleBadgeAdmin}`,
  operator: `${styles.roleBadge} ${styles.roleBadgeOperator}`,
  viewer: `${styles.roleBadge} ${styles.roleBadgeViewer}`,
};

export function UsersPage({ onAuthRequired }: { onAuthRequired?: () => void }) {
  const { t } = useTranslation();
  const [users, setUsers] = useState<User[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState<{ kind: 'success' | 'error'; message: string } | null>(null);
  const [showCreate, setShowCreate] = useState(false);
  const [editingUser, setEditingUser] = useState<User | null>(null);

  const loadUsers = useCallback(async () => {
    setLoading(true);
    setError('');
    try {
      const rows = await fetchUsers();
      setUsers(rows);
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        onAuthRequired?.();
        return;
      }
      setError(err instanceof Error ? err.message : 'Failed to load users');
    } finally {
      setLoading(false);
    }
  }, [onAuthRequired]);

  useEffect(() => {
    void loadUsers();
  }, [loadUsers]);

  const handleDelete = useCallback(async (user: User) => {
    if (!window.confirm(t('users.delete_confirm', { username: user.username }))) return;
    try {
      await deleteUser(user.id);
      setNotice({ kind: 'success', message: t('users.deleted') });
      void loadUsers();
    } catch (err) {
      if (err instanceof ApiError && err.status === 400) {
        setNotice({ kind: 'error', message: t('users.cannot_delete_self') });
        return;
      }
      if (err instanceof ApiError && err.status === 401) {
        onAuthRequired?.();
        return;
      }
      setNotice({ kind: 'error', message: err instanceof Error ? err.message : t('users.delete_failed') });
    }
  }, [loadUsers, onAuthRequired, t]);

  useEffect(() => {
    if (!notice) return;
    const timer = window.setTimeout(() => setNotice(null), 4000);
    return () => window.clearTimeout(timer);
  }, [notice]);

  return (
    <div className={styles.pageShell}>
      <div className={styles.pageFrame}>
        <div className={styles.header}>
          <div>
            <h2 className={styles.title}>{t('users.title')}</h2>
            <p className={styles.subtitle}>{t('users.subtitle')}</p>
          </div>
          <div className={styles.headerActions}>
            <Button variant="ghost" onClick={() => { window.location.href = appPath('/'); }}>
              {t('users.back_to_dashboard')}
            </Button>
            <Button variant="ghost" onClick={() => { window.location.href = appPath('/contentfilter'); }}>
              {t('content_filter.title')}
            </Button>
            <Button onClick={() => setShowCreate(true)}>
              {t('users.new_user')}
            </Button>
          </div>
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
        ) : (
          <div className={styles.tableContainer}>
            <table className={styles.table}>
              <thead>
                <tr>
                  <th>{t('users.username')}</th>
                  <th>{t('users.email')}</th>
                  <th>{t('users.role')}</th>
                  <th>{t('users.api_key')}</th>
                  <th>{t('users.quota')}</th>
                  <th>{t('users.used')}</th>
                  <th>{t('users.status')}</th>
                  <th>{t('users.actions')}</th>
                </tr>
              </thead>
              <tbody>
                {users.length === 0 && (
                  <tr>
                    <td colSpan={8} className={styles.emptyCell}>
                      {t('users.empty')}
                    </td>
                  </tr>
                )}
                {users.map((user) => (
                  <tr key={user.id}>
                    <td><strong>{user.username}</strong></td>
                    <td>{user.email}</td>
                    <td>
                      <span className={ROLE_BADGE_CLASSES[user.role] ?? styles.roleBadge}>
                        {t(`users.role_${user.role}`)}
                      </span>
                    </td>
                    <td>
                      <code className={styles.apiKeyCell}>{user.api_key ?? '—'}</code>
                    </td>
                    <td>{user.quota < 0 ? t('users.unlimited') : user.quota.toLocaleString()}</td>
                    <td>{user.used.toLocaleString()}</td>
                    <td>
                      <span className={user.active ? styles.statusActive : styles.statusInactive}>
                        {user.active ? t('users.active') : t('users.inactive')}
                      </span>
                    </td>
                    <td>
                      <div className={styles.actions}>
                        <Button variant="ghost" size="sm" onClick={() => setEditingUser(user)}>
                          {t('users.edit_title')}
                        </Button>
                        <Button variant="danger" size="sm" onClick={() => void handleDelete(user)}>
                          {t('common.delete')}
                        </Button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      {showCreate && (
        <UserFormModal
          key="create"
          onClose={() => setShowCreate(false)}
          onSaved={() => {
            setShowCreate(false);
            setNotice({ kind: 'success', message: t('users.create_success') });
            void loadUsers();
          }}
          onAuthRequired={onAuthRequired}
        />
      )}

      {editingUser && (
        <UserFormModal
          key={editingUser.id}
          user={editingUser}
          onClose={() => setEditingUser(null)}
          onSaved={() => {
            setEditingUser(null);
            setNotice({ kind: 'success', message: t('users.update_success') });
            void loadUsers();
          }}
          onAuthRequired={onAuthRequired}
        />
      )}
    </div>
  );
}

interface UserFormModalProps {
  user?: User;
  onClose: () => void;
  onSaved: () => void;
  onAuthRequired?: () => void;
}

function UserFormModal({ user, onClose, onSaved, onAuthRequired }: UserFormModalProps) {
  const { t } = useTranslation();
  const [username, setUsername] = useState(user?.username ?? '');
  const [email, setEmail] = useState(user?.email ?? '');
  const [password, setPassword] = useState('');
  const [role, setRole] = useState<UserRole>(user?.role ?? 'viewer');
  const [quota, setQuota] = useState(String(user?.quota ?? '0'));
  const [active, setActive] = useState(user?.active ?? true);
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState('');

  const roleOptions = ROLE_OPTIONS.map(({ value, labelKey }) => ({ value, label: t(labelKey) }));

  const handleSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setSaving(true);
    setErr('');
    try {
      if (user) {
        const payload: UserUpdateRequest = {};
        if (email !== user.email) payload.email = email;
        if (password) payload.password = password;
        if (role !== user.role) payload.role = role;
        const q = Number(quota);
        if (q !== user.quota) payload.quota = q;
        if (active !== user.active) payload.active = active;
        await updateUser(user.id, payload);
      } else {
        await createUser({
          username,
          email,
          password,
          role,
          quota: Number(quota),
        });
      }
      onSaved();
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) {
        onAuthRequired?.();
        return;
      }
      setErr(error instanceof Error ? error.message : t('users.save_failed'));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Modal
      open
      title={user ? t('users.edit_title') : t('users.create_title')}
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={saving}>
            {t('users.cancel')}
          </Button>
          <Button type="submit" form="user-form" loading={saving}>
            {saving ? t('users.saving') : t('users.save')}
          </Button>
        </>
      }
    >
      <form id="user-form" className={styles.form} onSubmit={(event) => void handleSubmit(event)}>
        {err && <div className={styles.errorBox}>{err}</div>}
        {!user && (
          <Input
            label={t('users.username_label')}
            value={username}
            onChange={(event) => setUsername(event.target.value)}
            required
            minLength={2}
          />
        )}
        <Input
          type="email"
          label={t('users.email_label')}
          value={email}
          onChange={(event) => setEmail(event.target.value)}
          required
        />
        <Input
          type="password"
          label={user ? t('users.password_optional_label') : t('users.password_label')}
          value={password}
          onChange={(event) => setPassword(event.target.value)}
          required={!user}
          minLength={user ? 0 : 6}
          hint={user ? t('users.new_password_hint') : undefined}
        />
        <div className={styles.formField}>
          <span className={styles.formLabel}>{t('users.role_label')}</span>
          <Select
            value={role}
            options={roleOptions}
            onChange={(value) => setRole(value as UserRole)}
            ariaLabel={t('users.role_label')}
          />
        </div>
        <Input
          type="number"
          label={t('users.quota_label')}
          value={quota}
          onChange={(event) => setQuota(event.target.value)}
        />
        {user && (
          <label className={styles.checkboxRow}>
            <input
              type="checkbox"
              checked={active}
              onChange={(event) => setActive(event.target.checked)}
            />
            <span>{t('users.active_label')}</span>
          </label>
        )}
      </form>
    </Modal>
  );
}