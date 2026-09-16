import { Navigate, Route, Routes } from 'react-router-dom';
import { getToken } from './api/client';
import AdminLayout from './layout/AdminLayout';
import Login from './pages/Login';
import Dashboard from './pages/Dashboard';
import Providers from './pages/Providers';
import Accounts from './pages/Accounts';
import Models from './pages/Models';
import ModelRoutes from './pages/ModelRoutes';
import ClientKeys from './pages/ClientKeys';
import Logs from './pages/Logs';
import Settings from './pages/Settings';

function RequireAuth({ children }: { children: JSX.Element }) {
  if (!getToken()) return <Navigate to="/login" replace />;
  return children;
}

export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      <Route
        path="/"
        element={
          <RequireAuth>
            <AdminLayout />
          </RequireAuth>
        }
      >
        <Route index element={<Dashboard />} />
        <Route path="providers" element={<Providers />} />
        <Route path="accounts" element={<Accounts />} />
        <Route path="models" element={<Models />} />
        <Route path="model-routes" element={<ModelRoutes />} />
        <Route path="client-keys" element={<ClientKeys />} />
        <Route path="logs" element={<Logs />} />
        <Route path="settings" element={<Settings />} />
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}
