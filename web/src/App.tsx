import { Navigate, Route, Routes } from 'react-router-dom';
import { getToken } from './api/client';
import AdminLayout from './layout/AdminLayout';
import Login from './pages/Login';
import Dashboard from './pages/Dashboard';
import Providers from './pages/Providers';
import Models from './pages/Models';
import Aliases from './pages/Aliases';
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
        <Route path="models" element={<Models />} />
        <Route path="aliases" element={<Aliases />} />
        <Route path="client-keys" element={<ClientKeys />} />
        <Route path="logs" element={<Logs />} />
        <Route path="settings" element={<Settings />} />
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}
